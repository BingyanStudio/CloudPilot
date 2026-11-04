package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/gorm"
)

type interactiveCall struct {
	Task     Task
	Session  model.AgentSession
	Turn     model.AgentTurn
	Controls <-chan SessionControl
	Emit     func(SessionUpdate)
	Finish   chan bool
}

type interactiveExecutor struct {
	started               chan *interactiveCall
	active                atomic.Int32
	maximum               atomic.Int32
	setup                 bool
	close                 func() error
	turnError             error
	transportCancellation bool
}

func (*interactiveExecutor) Execute(context.Context, Task, func(string, string)) ([]model.Artifact, error) {
	return nil, errors.New("unexpected batch execution")
}
func (*interactiveExecutor) SupportsInteraction(Task) bool { return true }
func (f *interactiveExecutor) CloseSession(context.Context, Task, model.AgentSession) error {
	if f.close != nil {
		return f.close()
	}
	return nil
}
func (f *interactiveExecutor) ExecuteTurn(ctx context.Context, task Task, session model.AgentSession, turn model.AgentTurn, controls <-chan SessionControl, emit func(SessionUpdate), _ func(string, string)) (TurnResult, error) {
	if f.setup {
		return TurnResult{}, errors.New("worker setup unavailable")
	}
	current := f.active.Add(1)
	defer f.active.Add(-1)
	for maximum := f.maximum.Load(); current > maximum && !f.maximum.CompareAndSwap(maximum, current); maximum = f.maximum.Load() {
	}
	result := TurnResult{ThreadID: fmt.Sprintf("thread-%d", session.ID), ProviderTurnID: fmt.Sprintf("turn-%d", turn.ID)}
	emit(SessionUpdate{ThreadID: result.ThreadID, ProviderTurnID: result.ProviderTurnID})
	call := &interactiveCall{Task: task, Session: session, Turn: turn, Controls: controls, Emit: emit, Finish: make(chan bool, 1)}
	f.started <- call
	select {
	case result.Interrupted = <-call.Finish:
		return result, f.turnError
	case <-ctx.Done():
		if f.transportCancellation {
			return result, errors.New("docker attach: signal killed")
		}
		return result, ctx.Err()
	}
}

func nextTurn(t *testing.T, executor *interactiveExecutor) *interactiveCall {
	t.Helper()
	select {
	case call := <-executor.started:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("interactive turn did not start")
		return nil
	}
}

func waitSession(t *testing.T, db *gorm.DB, runID uint, state string) model.AgentSession {
	t.Helper()
	var session model.AgentSession
	eventually(t, func() bool {
		return db.Where("run_id = ?", runID).First(&session).Error == nil && session.State == state
	})
	return session
}

func updatePipeline(t *testing.T, db *gorm.DB, paused bool) {
	t.Helper()
	r, err := model.FindResource(db, "Pipeline", "feedback")
	if err != nil {
		t.Fatal(err)
	}
	var spec model.PipelineSpec
	_ = r.Decode(&spec)
	spec.Paused = paused
	r.Spec, _ = json.Marshal(spec)
	if err := db.Save(&r).Error; err != nil {
		t.Fatal(err)
	}
}

// TestSessionUsageSurvivesResume keeps cumulative usage independent of repeated reports and turn outcomes.
func TestSessionUsageSurvivesResume(t *testing.T) {
	executor := &interactiveExecutor{started: make(chan *interactiveCall, 2)}
	db, scheduler := fixture(t, model.PipelineSpec{Concurrency: 1, TimeoutSeconds: 30, MaxRetries: 2}, executor)
	enqueue(t, db, time.Now().UTC())
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := nextTurn(t, executor)
	at := time.Now().UTC()
	first.Emit(SessionUpdate{ActivityAt: at, Usage: &TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}})
	first.Emit(SessionUpdate{ActivityAt: at.Add(-time.Second), Usage: &TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}})
	first.Emit(SessionUpdate{Usage: &TokenUsage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}})
	if err := scheduler.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	retained := waitSession(t, db, first.Task.Run.ID, "interrupted")
	threadID := retained.ThreadID
	if retained.TotalTokens != 15 || retained.InputTokens != 10 || retained.OutputTokens != 5 || retained.UsageUpdatedAt == nil || retained.LastActivityAt == nil || !retained.LastActivityAt.Equal(at) {
		t.Fatalf("usage was lost or double-counted: %+v", retained)
	}
	restarted := Create(db, executor)
	restarted.PollInterval = 5 * time.Millisecond
	if err := restarted.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Stop(context.Background()) })
	if err := restarted.Resume(first.Task.Run.ID, "continue", 1); err != nil {
		t.Fatal(err)
	}
	second := nextTurn(t, executor)
	second.Emit(SessionUpdate{Usage: &TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}})
	second.Emit(SessionUpdate{Usage: &TokenUsage{InputTokens: 20, OutputTokens: 8, TotalTokens: 28}})
	second.Finish <- false
	retained = waitSession(t, db, first.Task.Run.ID, "idle")
	if retained.TotalTokens != 28 || retained.InputTokens != 20 || retained.OutputTokens != 8 || retained.ThreadID != threadID {
		t.Fatalf("resumed usage was added instead of replaced: %+v", retained)
	}
}

func TestFollowupUsesSnapshotAndHonorsConcurrencyAndPause(t *testing.T) {
	executor := &interactiveExecutor{started: make(chan *interactiveCall, 4)}
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1, TimeoutSeconds: 30}, executor)
	firstEvent := enqueue(t, db, time.Now().UTC())
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := nextTurn(t, executor)
	first.Finish <- false
	waitSession(t, db, first.Task.Run.ID, "idle")
	if eventStatus(t, db, firstEvent.ID) != "awaiting_review" {
		t.Fatal("natural completion must wait for task confirmation")
	}
	updatePipeline(t, db, true)
	workspace, _ := model.FindResource(db, "Workspace", "workspace")
	workspace.Spec = json.RawMessage(`{"prompt":"changed after the initial run"}`)
	if err := db.Save(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if err := engine.Resume(first.Task.Run.ID, "continue with corrected feedback", 9); err != nil {
		t.Fatal(err)
	}
	secondEvent := enqueue(t, db, time.Now().UTC())
	if err := engine.tick(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if eventStatus(t, db, firstEvent.ID) != "waiting" || len(executor.started) != 0 {
		t.Fatal("paused pipeline started a queued follow-up")
	}
	if err := engine.Resume(first.Task.Run.ID, "duplicate", 9); err == nil {
		t.Fatal("accepted two simultaneous follow-up turns")
	}
	updatePipeline(t, db, false)
	engine.notify()
	followup := nextTurn(t, executor)
	if followup.Task.Run.ID != first.Task.Run.ID || followup.Session.ID != first.Session.ID || followup.Turn.Number != 2 || followup.Turn.UserID != 9 || followup.Task.Workspace.Prompt != "" {
		t.Fatalf("follow-up lost identity or immutable configuration: %+v", followup)
	}
	if eventStatus(t, db, secondEvent.ID) != "queued" || executor.maximum.Load() != 1 {
		t.Fatal("follow-up exceeded pipeline concurrency")
	}
	if _, err := engine.Retry(firstEvent.ID); err == nil {
		t.Fatal("fresh retry accepted for an open session")
	}
	followup.Finish <- false
	second := nextTurn(t, executor)
	if second.Task.Event.ID != secondEvent.ID || second.Task.Run.ID == first.Task.Run.ID || second.Task.Workspace.Prompt != "changed after the initial run" {
		t.Fatal("new event did not receive fresh resource configuration")
	}
	second.Finish <- false
	waitSession(t, db, second.Task.Run.ID, "idle")
	if eventStatus(t, db, firstEvent.ID) != "awaiting_review" {
		t.Fatal("successful follow-up must wait for task confirmation again")
	}
}

func TestCloseConfirmsSuccessfulTaskOnlyAfterCleanup(t *testing.T) {
	for _, test := range []struct {
		name, state, before, after string
		turnError                  error
		interrupted                bool
	}{
		{name: "success", state: "idle", before: "awaiting_review", after: "succeeded"},
		{name: "failure", state: "failed", before: "failed", after: "failed", turnError: errors.New("worker failed")},
		{name: "interruption", state: "interrupted", before: "interrupted", after: "cancelled", interrupted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var closes atomic.Int32
			executor := &interactiveExecutor{started: make(chan *interactiveCall, 1), turnError: test.turnError, close: func() error {
				if closes.Add(1) == 1 {
					return errors.New("workspace cleanup unavailable")
				}
				return nil
			}}
			db, scheduler := fixture(t, model.PipelineSpec{Concurrency: 1}, executor)
			event := enqueue(t, db, time.Now().UTC())
			if err := scheduler.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			call := nextTurn(t, executor)
			call.Finish <- test.interrupted
			waitSession(t, db, call.Task.Run.ID, test.state)
			if status := eventStatus(t, db, event.ID); status != test.before {
				t.Fatalf("event status before close = %s, want %s", status, test.before)
			}
			if err := scheduler.CloseSession(call.Task.Run.ID); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool {
				session, capabilities, err := scheduler.SessionInfo(call.Task.Run.ID)
				return err == nil && session.State == "closing" && session.LastError != "" && capabilities.CanClose
			})
			if status := eventStatus(t, db, event.ID); status == "succeeded" {
				t.Fatal("cleanup failure confirmed the task")
			}
			if err := scheduler.CloseSession(call.Task.Run.ID); err != nil {
				t.Fatal(err)
			}
			waitSession(t, db, call.Task.Run.ID, "closed")
			if status := eventStatus(t, db, event.ID); status != test.after {
				t.Fatalf("event status after close = %s, want %s", status, test.after)
			}
		})
	}
}

func TestUnacknowledgedSteerIsRejectedAndRecoveryPreservesQueuedTurn(t *testing.T) {
	executor := &interactiveExecutor{started: make(chan *interactiveCall, 4)}
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 2, TimeoutSeconds: 30, MaxRetries: 3}, executor)
	enqueue(t, db, time.Now().UTC())
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := nextTurn(t, executor)
	if err := engine.SendMessage(first.Task.Run.ID, "information raced with completion", 5); err != nil {
		t.Fatal(err)
	}
	control := <-first.Controls
	first.Finish <- true
	waitSession(t, db, first.Task.Run.ID, "interrupted")
	var acknowledgement model.SessionEvent
	if err := db.Where("control_id = ? AND type = ?", control.ID, "control").First(&acknowledgement).Error; err != nil || acknowledgement.Status != "rejected" {
		t.Fatalf("unconfirmed input falsely accepted: %+v %v", acknowledgement, err)
	}
	updatePipeline(t, db, true)
	if err := engine.Resume(first.Task.Run.ID, "queued before restart", 5); err != nil {
		t.Fatal(err)
	}
	if err := engine.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted := Create(db, executor)
	restarted.PollInterval = 5 * time.Millisecond
	if err := restarted.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Stop(context.Background()) })
	session := waitSession(t, db, first.Task.Run.ID, "queued")
	if session.ThreadID == "" || eventStatus(t, db, first.Task.Event.ID) != "waiting" {
		t.Fatal("restart discarded the pending follow-up or provider thread")
	}
	updatePipeline(t, db, false)
	restarted.notify()
	next := nextTurn(t, executor)
	if next.Turn.Input != "queued before restart" || next.Turn.Number != 2 {
		t.Fatal("restart replayed initial feedback instead of pending human input")
	}
	if err := restarted.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitSession(t, db, first.Task.Run.ID, "interrupted")
	if eventStatus(t, db, first.Task.Event.ID) != "interrupted" {
		t.Fatal("shutdown automatically retried an established conversation")
	}
}

func TestSetupRetryCleanupBlocksResumeAndCleanupFailureRemainsRetryable(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanupFails=%t", cleanupFails), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var calls atomic.Int32
			defer once.Do(func() { close(release) })
			executor := &interactiveExecutor{setup: true, close: func() error {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
					if cleanupFails {
						return errors.New("daemon unavailable")
					}
				}
				return nil
			}}
			db, engine := fixture(t, model.PipelineSpec{Concurrency: 1, MaxRetries: 1}, executor)
			event := enqueue(t, db, time.Now().UTC())
			if err := engine.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("setup failure did not clean up before retry")
			}
			var session model.AgentSession
			if err := db.First(&session).Error; err != nil {
				t.Fatal(err)
			}
			_, caps, err := engine.SessionInfo(session.RunID)
			if err != nil || caps.CanResume || caps.CanSend || engine.Resume(session.RunID, "race cleanup", 1) == nil {
				t.Fatal("human turn accepted while retry cleanup was removing its workspace")
			}
			once.Do(func() { close(release) })
			if cleanupFails {
				eventually(t, func() bool {
					value, caps, err := engine.SessionInfo(session.RunID)
					return err == nil && value.State == "closing" && value.LastError != "" && caps.CanClose
				})
				if eventStatus(t, db, event.ID) != "failed" {
					t.Fatal("failed cleanup still requeued the event")
				}
				if err := engine.CloseSession(session.RunID); err != nil {
					t.Fatal(err)
				}
				waitSession(t, db, session.RunID, "closed")
			} else {
				waitSession(t, db, session.RunID, "closed")
				eventually(t, func() bool { return eventStatus(t, db, event.ID) == "queued" })
			}
		})
	}
}

func TestRejectedInterruptAndSteerKeepDeliveryHonest(t *testing.T) {
	executor := &interactiveExecutor{started: make(chan *interactiveCall, 1)}
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1, TimeoutSeconds: 30}, executor)
	enqueue(t, db, time.Now().UTC())
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := nextTurn(t, executor)
	message := strings.Repeat("x", 32<<10)
	if err := engine.SendMessage(call.Task.Run.ID, message, 7); err != nil {
		t.Fatal(err)
	}
	control := <-call.Controls
	if control.Text != message {
		t.Fatal("human input was truncated")
	}
	call.Emit(SessionUpdate{ControlID: control.ID, ControlError: "turn changed before steering"})
	var entry model.SessionEvent
	if err := db.First(&entry, control.ID).Error; err != nil || entry.Text != message {
		t.Fatal("persisted human input was truncated")
	}
	if err := engine.Interrupt(call.Task.Run.ID); err != nil {
		t.Fatal(err)
	}
	control = <-call.Controls
	if control.Type != "interrupt" {
		t.Fatal("missing interrupt control")
	}
	call.Emit(SessionUpdate{ControlError: "provider rejected interruption"})
	waitSession(t, db, call.Task.Run.ID, "running")
	_, caps, err := engine.SessionInfo(call.Task.Run.ID)
	if err != nil || !caps.CanSend || !caps.CanInterrupt {
		t.Fatal("rejected interrupt left the session stuck")
	}
	if err := engine.CloseSession(call.Task.Run.ID); err != nil {
		t.Fatal(err)
	}
	waitSession(t, db, call.Task.Run.ID, "closed")
	if eventStatus(t, db, call.Task.Event.ID) != "cancelled" {
		t.Fatal("explicit close did not end the active event")
	}
}

func TestCrashRecoveryInterruptsConversationWithoutAutomaticReplay(t *testing.T) {
	executor := &interactiveExecutor{started: make(chan *interactiveCall, 1)}
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1, MaxRetries: 5}, executor)
	enqueue(t, db, time.Now().UTC())
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := nextTurn(t, executor)
	if err := engine.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	updatePipeline(t, db, true)
	// Recreate the durable records left when a process exits before persisting its final turn state.
	for _, item := range []struct {
		value   any
		updates map[string]any
	}{
		{&call.Session, map[string]any{"state": "running"}},
		{&call.Turn, map[string]any{"status": "running", "finished_at": nil}},
		{&call.Task.Run, map[string]any{"status": "running", "finished_at": nil}},
		{&call.Task.Event, map[string]any{"status": "running"}},
	} {
		if err := db.Model(item.value).Updates(item.updates).Error; err != nil {
			t.Fatal(err)
		}
	}
	pending := model.SessionEvent{RunID: call.Task.Run.ID, TurnID: call.Turn.ID, Type: "message", Role: "user", Status: "queued", Text: "delivery unknown"}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	restarted := Create(db, executor)
	if err := restarted.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Stop(context.Background()) })
	session := waitSession(t, db, call.Task.Run.ID, "interrupted")
	if session.ThreadID == "" || eventStatus(t, db, call.Task.Event.ID) != "interrupted" {
		t.Fatal("crash recovery lost conversation or requeued it automatically")
	}
	var acknowledgement model.SessionEvent
	if err := db.Where("control_id = ? AND type = ?", pending.ID, "control").First(&acknowledgement).Error; err != nil || acknowledgement.Status != "rejected" {
		t.Fatal("recovered unacknowledged message was presented as delivered")
	}
}

func TestShutdownTransportErrorRemainsInterrupted(t *testing.T) {
	executor := &interactiveExecutor{started: make(chan *interactiveCall, 1), transportCancellation: true}
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1, MaxRetries: 2}, executor)
	event := enqueue(t, db, time.Now().UTC())
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := nextTurn(t, executor)
	if err := engine.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	session := waitSession(t, db, call.Task.Run.ID, "interrupted")
	if session.ThreadID == "" || eventStatus(t, db, event.ID) != "interrupted" {
		t.Fatal("transport error changed shutdown into failed execution")
	}
}
