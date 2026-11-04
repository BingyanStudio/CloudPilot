package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/gorm"
)

func (e *Engine) startSession(task Task, now time.Time) error {
	snapshot, err := json.Marshal(model.ExecutionSnapshot{Pipeline: task.Pipeline, Worker: task.Worker, Workspace: task.Workspace, Runner: task.Runner})
	if err != nil {
		return err
	}
	session := model.AgentSession{RunID: task.Run.ID, State: "starting", Snapshot: snapshot}
	turn := model.AgentTurn{RunID: task.Run.ID, Number: 1, Input: string(task.Event.Payload), Initial: true, Status: "running", StartedAt: &now}
	if err := e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		turn.SessionID = session.ID
		if err := tx.Create(&turn).Error; err != nil {
			return err
		}
		session.CurrentTurnID = turn.ID
		if err := tx.Save(&session).Error; err != nil {
			return err
		}
		return tx.Create(&model.SessionEvent{RunID: task.Run.ID, TurnID: turn.ID, Type: "message", Role: "user", Status: "accepted", Text: turn.Input, ItemID: fmt.Sprintf("turn-input-%d", turn.ID)}).Error
	}); err != nil {
		return err
	}
	e.launchTurn(task, session, turn)
	return nil
}

func (e *Engine) dispatchTurns(pipeline string, concurrency int, running int64, now time.Time) (int64, error) {
	for running < int64(concurrency) {
		var task Task
		var session model.AgentSession
		var turn model.AgentTurn
		if err := e.DB.Joins("JOIN runs ON runs.id = agent_turns.run_id").Joins("JOIN events ON events.id = runs.event_id").Where("runs.pipeline_name = ? AND agent_turns.status = ? AND events.available_at <= ?", pipeline, "queued", now).Order("events.available_at, agent_turns.id").First(&turn).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				break
			}
			return running, err
		}
		if err := e.DB.First(&session, turn.SessionID).Error; err != nil {
			return running, err
		}
		task, err := e.sessionTask(e.DB, session)
		if err != nil {
			return running, err
		}
		eligible, checkErr := e.checkGitHub(task.Event)
		if errors.Is(checkErr, errSourceCheckBudget) {
			break
		}
		if checkErr != nil {
			if errors.Is(checkErr, context.Canceled) {
				return running, checkErr
			}
			if err := e.deferGitHubCheck(task.Event, checkErr); err != nil {
				return running, err
			}
			continue
		}
		if !eligible {
			if err := e.rejectGitHubTurn(task, session, turn, now); err != nil {
				return running, err
			}
			continue
		}
		allowed, err := e.canDispatch(pipeline)
		if err != nil || !allowed {
			return running, err
		}
		if err := e.DB.First(&session, turn.SessionID).Error; err != nil {
			return running, err
		}
		if session.State != "queued" || session.CurrentTurnID != turn.ID {
			continue
		}
		err = e.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.First(&session, turn.SessionID).Error; err != nil {
				return err
			}
			if session.State != "queued" || session.CurrentTurnID != turn.ID {
				return errors.New("queued turn does not match session state")
			}
			executor, ok := e.Executor.(InteractiveExecutor)
			if !ok || !executor.SupportsInteraction(task) {
				return errors.New("session worker is unavailable")
			}
			turn.Status, turn.StartedAt = "running", &now
			if err := tx.Save(&turn).Error; err != nil {
				return err
			}
			session.State = "starting"
			if err := tx.Save(&session).Error; err != nil {
				return err
			}
			task.Run.Status, task.Run.FinishedAt, task.Run.Error = "running", nil, ""
			if err := tx.Save(&task.Run).Error; err != nil {
				return err
			}
			task.Event.Status = "running"
			return tx.Save(&task.Event).Error
		})
		if errors.Is(err, gorm.ErrRecordNotFound) {
			break
		}
		if err != nil {
			return running, err
		}
		e.launchTurn(task, session, turn)
		running++
	}
	return running, nil
}

func (e *Engine) launchTurn(task Task, session model.AgentSession, turn model.AgentTurn) {
	seconds := task.Pipeline.TimeoutSeconds
	if seconds < 1 {
		seconds = 3600
	}
	ctx, cancel := context.WithTimeout(e.ctx, time.Duration(seconds)*time.Second)
	active := &execution{cancel: cancel, sessionID: session.ID, controls: make(chan SessionControl, 32), pending: map[uint]bool{}, eventID: task.Event.ID, sourceCheckAt: time.Now().UTC().Add(sourceCheckInterval)}
	e.active[task.Run.ID] = active
	e.wg.Go(func() { e.executeTurn(ctx, task, session, turn, active) })
}

func (e *Engine) executeTurn(ctx context.Context, task Task, session model.AgentSession, turn model.AgentTurn, active *execution) {
	executor := e.Executor.(InteractiveExecutor)
	count, size, limited := 0, 0, false
	emit := func(update SessionUpdate) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.active[task.Run.ID] != active {
			return
		}
		if update.ThreadID != "" && session.ThreadID != "" && update.ThreadID != session.ThreadID {
			active.failure = "worker changed the session thread ID"
			active.cancel()
			return
		}
		if update.ThreadID != "" {
			session.ThreadID = update.ThreadID
		}
		if update.ProviderTurnID != "" {
			turn.ProviderTurnID = update.ProviderTurnID
		}
		err := e.DB.Transaction(func(tx *gorm.DB) error {
			telemetry := map[string]any{}
			if !update.ActivityAt.IsZero() && (session.LastActivityAt == nil || update.ActivityAt.After(*session.LastActivityAt)) {
				activity := update.ActivityAt.UTC()
				session.LastActivityAt = &activity
				telemetry["last_activity_at"] = activity
			}
			if usage := update.Usage; usage != nil && session.ThreadID != "" && usage.InputTokens >= session.InputTokens && usage.OutputTokens >= session.OutputTokens && usage.TotalTokens >= session.TotalTokens {
				now := time.Now().UTC()
				session.InputTokens, session.OutputTokens, session.TotalTokens = usage.InputTokens, usage.OutputTokens, usage.TotalTokens
				session.UsageUpdatedAt = &now
				telemetry["input_tokens"], telemetry["output_tokens"], telemetry["total_tokens"] = usage.InputTokens, usage.OutputTokens, usage.TotalTokens
				telemetry["usage_updated_at"] = now
			}
			if len(telemetry) > 0 {
				if err := tx.Model(&session).Updates(telemetry).Error; err != nil {
					return err
				}
			}
			if update.ThreadID != "" {
				if err := tx.Model(&session).Update("thread_id", update.ThreadID).Error; err != nil {
					return err
				}
			}
			if update.ProviderTurnID != "" {
				if err := tx.Model(&turn).Update("provider_turn_id", update.ProviderTurnID).Error; err != nil {
					return err
				}
				if err := tx.Model(&session).Where("state = ?", "starting").Update("state", "running").Error; err != nil {
					return err
				}
			}
			if update.ControlID == 0 && update.ControlError != "" {
				if err := tx.Model(&session).Where("state = ?", "interrupting").Updates(map[string]any{"state": "running", "last_error": update.ControlError}).Error; err != nil {
					return err
				}
				if err := tx.Create(&model.SessionEvent{RunID: task.Run.ID, TurnID: turn.ID, Type: "error", Status: "failed", Text: "Could not interrupt turn: " + update.ControlError}).Error; err != nil {
					return err
				}
			}
			if update.ControlID != 0 && active.pending[update.ControlID] {
				status := "accepted"
				if update.ControlError != "" {
					status = "rejected"
				}
				if err := tx.Create(&model.SessionEvent{RunID: task.Run.ID, TurnID: turn.ID, Type: "control", ControlID: update.ControlID, Status: status, Text: update.ControlError}).Error; err != nil {
					return err
				}
			}
			if update.Event != nil && !limited {
				entry := *update.Event
				entry.ID, entry.RunID, entry.TurnID = 0, task.Run.ID, turn.ID
				entry.Text, entry.Output = boundedText(entry.Text, 1<<20), boundedText(entry.Output, 256<<10)
				entry.Command = boundedText(entry.Command, 32<<10)
				entry.ItemID, entry.Tool = boundedText(entry.ItemID, 190), boundedText(entry.Tool, 190)
				size += len(entry.Text) + len(entry.Output) + len(entry.Command)
				count++
				if count > 10000 || size > 16<<20 {
					limited = true
					entry = model.SessionEvent{RunID: task.Run.ID, TurnID: turn.ID, Type: "status", Status: "truncated", Text: "Conversation output limit reached for this turn; further provider output is omitted."}
				}
				return tx.Create(&entry).Error
			}
			return nil
		})
		if err != nil {
			active.failure = "could not persist session progress"
			active.cancel()
			slog.Error("persist session progress failed", "run", task.Run.ID, "error", err)
		} else if update.ControlID != 0 {
			delete(active.pending, update.ControlID)
		}
	}
	result, err := invokeTurn(executor, ctx, task, session, turn, active.controls, emit, e.runLogger(task.Run.ID))
	e.mu.Lock()
	contextErr := ctx.Err()
	active.cancel()
	closing := active.closing
	if result.ThreadID != "" {
		if session.ThreadID != "" && session.ThreadID != result.ThreadID {
			active.failure = "worker changed the session thread ID"
		} else {
			session.ThreadID = result.ThreadID
		}
	}
	if result.ProviderTurnID != "" {
		turn.ProviderTurnID = result.ProviderTurnID
	}
	state, status, turnStatus, message := "idle", "succeeded", "completed", ""
	if result.Interrupted || errors.Is(contextErr, context.Canceled) {
		state, status, turnStatus = "interrupted", "interrupted", "interrupted"
	}
	if err != nil && !result.Interrupted && !errors.Is(contextErr, context.Canceled) && !errors.Is(err, context.Canceled) {
		state, status, turnStatus, message = "failed", "failed", "failed", err.Error()
	}
	if errors.Is(contextErr, context.DeadlineExceeded) {
		state, status, turnStatus, message = "failed", "failed", "failed", "execution exceeded pipeline timeout"
	}
	if active.failure != "" {
		state, status, turnStatus, message = "failed", "failed", "failed", active.failure
	}
	if active.sourceStopped != "" {
		state, status, turnStatus, message = "interrupted", "interrupted", "interrupted", active.sourceStopped
	}
	if closing {
		state, status, turnStatus, message = "closing", "cancelled", "interrupted", "session ended by operator"
	}
	autoRetry := state == "failed" && turn.Initial && session.ThreadID == "" && active.failure == "" && task.Event.Attempts <= task.Pipeline.MaxRetries
	if autoRetry {
		state = "closing"
	}
	if persistErr := e.finishTurn(task, session, turn, active, state, status, turnStatus, message, result.Artifacts); persistErr != nil {
		slog.Error("persist turn result failed", "run", task.Run.ID, "error", persistErr)
		if fallbackErr := e.finishTurn(task, session, turn, active, "failed", "failed", "failed", "could not persist turn result", nil); fallbackErr != nil {
			slog.Error("persist turn failure failed", "run", task.Run.ID, "error", fallbackErr)
		}
	}
	delete(e.active, task.Run.ID)
	if closing || autoRetry {
		e.closing[task.Run.ID] = true
	}
	e.notify()
	e.mu.Unlock()
	if closing || autoRetry {
		if closeErr := e.releaseSession(task, session); closeErr == nil && autoRetry {
			e.mu.Lock()
			retryErr := e.DB.Model(&task.Event).Where("status = ?", "failed").Updates(map[string]any{"status": "queued", "available_at": time.Now().UTC().Add(retryDelay(task.Event.Attempts))}).Error
			if retryErr != nil {
				slog.Error("queue setup retry failed", "run", task.Run.ID, "error", retryErr)
			}
			e.notify()
			e.mu.Unlock()
		}
	}
}

func invokeTurn(executor InteractiveExecutor, ctx context.Context, task Task, session model.AgentSession, turn model.AgentTurn, controls <-chan SessionControl, emit func(SessionUpdate), log func(string, string)) (result TurnResult, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("interactive executor panic: %v", value)
		}
	}()
	return executor.ExecuteTurn(ctx, task, session, turn, controls, emit, log)
}

func boundedText(text string, maximum int) string {
	if len(text) <= maximum {
		return text
	}
	return strings.ToValidUTF8(text[:maximum-15], "") + " [truncated]"
}

func (e *Engine) finishTurn(task Task, session model.AgentSession, turn model.AgentTurn, active *execution, state, status, turnStatus, message string, artifacts []model.Artifact) error {
	now := time.Now().UTC()
	eventStatus := status
	if status == "succeeded" {
		eventStatus = "awaiting_review"
	}
	return e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&session).Updates(map[string]any{"state": state, "last_error": message, "thread_id": session.ThreadID}).Error; err != nil {
			return err
		}
		if err := tx.Model(&turn).Updates(map[string]any{"status": turnStatus, "provider_turn_id": turn.ProviderTurnID, "finished_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&task.Run).Updates(map[string]any{"status": status, "error": message, "finished_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&task.Event).Update("status", eventStatus).Error; err != nil {
			return err
		}
		for id := range active.pending {
			if err := tx.Create(&model.SessionEvent{RunID: task.Run.ID, TurnID: turn.ID, Type: "control", ControlID: id, Status: "rejected", Text: "Turn ended before the worker acknowledged this message; send it again to continue."}).Error; err != nil {
				return err
			}
		}
		for _, artifact := range artifacts {
			artifact.ID, artifact.RunID = 0, task.Run.ID
			if err := tx.Create(&artifact).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&model.RunLog{RunID: task.Run.ID, Stream: "system", Message: "Turn " + turnStatus + ": " + message}).Error; err != nil {
			return err
		}
		return tx.Create(&model.SessionEvent{RunID: task.Run.ID, TurnID: turn.ID, Type: "status", Status: turnStatus, Text: message}).Error
	})
}
