package engine

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/gorm"
)

// CloseSession ends interaction and releases the retained provider workspace.
func (e *Engine) CloseSession(runID uint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, err := e.sessionForAction(runID)
	if err != nil {
		return err
	}
	if session.State == "closed" || e.closing[runID] {
		return nil
	}
	task, err := e.sessionTask(e.DB, session)
	if err != nil {
		return err
	}
	if err := e.DB.Model(&session).Updates(map[string]any{"state": "closing", "last_error": ""}).Error; err != nil {
		return err
	}
	if active := e.active[runID]; active != nil {
		active.closing = true
		active.cancel()
		return nil
	}
	now := time.Now().UTC()
	if err := e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.AgentTurn{}).Where("session_id = ? AND status = ?", session.ID, "queued").Updates(map[string]any{"status": "interrupted", "finished_at": now}).Error; err != nil {
			return err
		}
		if task.Run.Status == "queued" || task.Run.Status == "interrupted" {
			if err := tx.Model(&task.Run).Updates(map[string]any{"status": "cancelled", "finished_at": now}).Error; err != nil {
				return err
			}
			return tx.Model(&task.Event).Update("status", "cancelled").Error
		}
		return nil
	}); err != nil {
		return err
	}
	e.closing[runID] = true
	e.wg.Go(func() { _ = e.releaseSession(task, session) })
	return nil
}

func (e *Engine) releaseSession(task Task, session model.AgentSession) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	executor, ok := e.Executor.(InteractiveExecutor)
	var err error
	if ok {
		err = executor.CloseSession(ctx, task, session)
	} else {
		err = errors.New("session worker is unavailable")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	defer delete(e.closing, task.Run.ID)
	state, message := "closed", ""
	updates := map[string]any{"state": state, "last_error": "", "closed_at": time.Now().UTC()}
	if err != nil {
		state, message = "closing", err.Error()
		updates = map[string]any{"state": state, "last_error": message}
	}
	persistErr := e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&session).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Create(&model.SessionEvent{RunID: task.Run.ID, TurnID: session.CurrentTurnID, Type: "status", Status: state, Text: message}).Error
	})
	if err != nil || persistErr != nil {
		slog.Error("close agent session failed", "run", task.Run.ID, "error", errors.Join(err, persistErr))
	}
	return errors.Join(err, persistErr)
}

func recoverSessions(tx *gorm.DB, now time.Time) error {
	var sessions []model.AgentSession
	if err := tx.Where("state IN ?", []string{"starting", "running", "interrupting", "closing"}).Find(&sessions).Error; err != nil {
		return err
	}
	for _, session := range sessions {
		var run model.Run
		if err := tx.First(&run, session.RunID).Error; err != nil {
			return err
		}
		state := "interrupted"
		if session.State == "closing" {
			state = "closing"
		}
		message := "Scheduler restarted; resume the retained session or end it explicitly."
		if err := tx.Model(&session).Updates(map[string]any{"state": state, "last_error": message}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.AgentTurn{}).Where("session_id = ? AND status IN ?", session.ID, []string{"running", "queued"}).Updates(map[string]any{"status": "interrupted", "finished_at": now}).Error; err != nil {
			return err
		}
		if run.Status == "running" || run.Status == "queued" {
			if err := tx.Model(&run).Updates(map[string]any{"status": "interrupted", "error": message, "finished_at": now}).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.Event{}).Where("id = ?", run.EventID).Update("status", "interrupted").Error; err != nil {
				return err
			}
		}
		var pending []model.SessionEvent
		if err := tx.Where("run_id = ? AND type = ? AND status = ? AND id NOT IN (?)", run.ID, "message", "queued", tx.Model(&model.SessionEvent{}).Select("control_id").Where("run_id = ? AND type = ?", run.ID, "control")).Find(&pending).Error; err != nil {
			return err
		}
		for _, entry := range pending {
			if err := tx.Create(&model.SessionEvent{RunID: run.ID, TurnID: entry.TurnID, Type: "control", ControlID: entry.ID, Status: "rejected", Text: "Delivery could not be confirmed after restart; check the conversation before resending."}).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&model.SessionEvent{RunID: run.ID, TurnID: session.CurrentTurnID, Type: "status", Status: state, Text: message}).Error; err != nil {
			return err
		}
	}
	return nil
}
