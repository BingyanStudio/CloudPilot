package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/gorm"
)

type SessionControl struct {
	ID   uint
	Type string
	Text string
}

type SessionUpdate struct {
	ThreadID       string
	ProviderTurnID string
	Event          *model.SessionEvent
	ControlID      uint
	ControlError   string
}

type TurnResult struct {
	ThreadID       string
	ProviderTurnID string
	Interrupted    bool
	Artifacts      []model.Artifact
}

// InteractiveExecutor preserves a worker conversation between independently scheduled turns.
type InteractiveExecutor interface {
	SupportsInteraction(Task) bool
	ExecuteTurn(context.Context, Task, model.AgentSession, model.AgentTurn, <-chan SessionControl, func(SessionUpdate), func(string, string)) (TurnResult, error)
	CloseSession(context.Context, Task, model.AgentSession) error
}

type Capabilities struct {
	Interactive  bool `json:"interactive"`
	CanSend      bool `json:"canSend"`
	CanInterrupt bool `json:"canInterrupt"`
	CanResume    bool `json:"canResume"`
	CanClose     bool `json:"canClose"`
}

// SessionInfo reports persisted state and actions currently supported by its worker.
func (e *Engine) SessionInfo(runID uint) (*model.AgentSession, Capabilities, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var run model.Run
	if err := e.DB.First(&run, runID).Error; err != nil {
		return nil, Capabilities{}, err
	}
	var session model.AgentSession
	if err := e.DB.Where("run_id = ?", runID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, Capabilities{}, nil
		}
		return nil, Capabilities{}, err
	}
	task, err := e.sessionTask(e.DB, session)
	if err != nil {
		return &session, Capabilities{}, err
	}
	executor, ok := e.Executor.(InteractiveExecutor)
	if !ok || !executor.SupportsInteraction(task) {
		return &session, Capabilities{}, nil
	}
	caps := Capabilities{Interactive: true}
	if e.started && !e.stopped {
		active := e.active[runID]
		caps.CanInterrupt = session.State == "running" && active != nil && active.controls != nil
		caps.CanResume = !session.WorkspaceLost && !e.closing[runID] && (session.State == "idle" || session.State == "interrupted" || session.State == "failed")
		caps.CanSend = caps.CanInterrupt || caps.CanResume
		caps.CanClose = session.State != "closed" && !e.closing[runID] && (active == nil || !active.closing)
	}
	return &session, caps, nil
}

// SendMessage steers an active turn or queues a follow-up on an idle session.
func (e *Engine) SendMessage(runID uint, text string, userID uint) error {
	if err := validateInput(text, false); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	session, err := e.sessionForAction(runID)
	if err != nil {
		return err
	}
	if session.State != "running" {
		return e.queueTurn(session, text, userID)
	}
	active := e.active[runID]
	if active == nil || active.controls == nil {
		return errors.New("session has no active turn")
	}
	if len(active.controls) == cap(active.controls) {
		return errors.New("too many pending session controls")
	}
	entry := model.SessionEvent{RunID: runID, TurnID: session.CurrentTurnID, Type: "message", Role: "user", Status: "queued", Text: text, ItemID: fmt.Sprintf("human-%d-%d", runID, time.Now().UnixNano())}
	if err := e.DB.Create(&entry).Error; err != nil {
		return err
	}
	active.pending[entry.ID] = true
	active.controls <- SessionControl{ID: entry.ID, Type: "message", Text: text}
	return nil
}

// Interrupt requests a worker-level interruption while retaining the session workspace.
func (e *Engine) Interrupt(runID uint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, err := e.sessionForAction(runID)
	if err != nil {
		return err
	}
	active := e.active[runID]
	if session.State != "running" || active == nil || active.controls == nil {
		return errors.New("only a running interactive turn can be interrupted")
	}
	if len(active.controls) == cap(active.controls) {
		return errors.New("too many pending session controls")
	}
	if err := e.DB.Model(&session).Updates(map[string]any{"state": "interrupting", "last_error": ""}).Error; err != nil {
		return err
	}
	active.controls <- SessionControl{Type: "interrupt"}
	return nil
}

// Resume schedules another turn in the same conversation and working directory.
func (e *Engine) Resume(runID uint, text string, userID uint) error {
	if err := validateInput(text, true); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		text = "Continue the task using the existing conversation and working directory. Check what has already been completed before taking further action."
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	session, err := e.sessionForAction(runID)
	if err != nil {
		return err
	}
	return e.queueTurn(session, text, userID)
}

func validateInput(text string, emptyAllowed bool) error {
	if len(text) > 32<<10 || strings.ContainsRune(text, '\x00') {
		return errors.New("message must not exceed 32 KiB or contain NUL")
	}
	if !emptyAllowed && strings.TrimSpace(text) == "" {
		return errors.New("message must not be empty")
	}
	return nil
}

func (e *Engine) sessionForAction(runID uint) (model.AgentSession, error) {
	var session model.AgentSession
	if !e.started || e.stopped {
		return session, errors.New("scheduler is not running")
	}
	var run model.Run
	if err := e.DB.First(&run, runID).Error; err != nil {
		return session, err
	}
	if err := e.DB.Where("run_id = ?", runID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return session, errors.New("run does not support interactive sessions")
		}
		return session, err
	}
	task, err := e.sessionTask(e.DB, session)
	if err != nil {
		return session, err
	}
	executor, ok := e.Executor.(InteractiveExecutor)
	if !ok || !executor.SupportsInteraction(task) {
		return session, errors.New("worker does not support interactive sessions")
	}
	return session, nil
}

func (e *Engine) queueTurn(session model.AgentSession, text string, userID uint) error {
	if session.WorkspaceLost {
		return errors.New("session workspace is unavailable; close this session and start a new run")
	}
	if e.closing[session.RunID] {
		return errors.New("session cleanup is in progress")
	}
	if session.State != "idle" && session.State != "interrupted" && session.State != "failed" {
		return fmt.Errorf("session is %s and cannot accept a new turn", session.State)
	}
	task, err := e.sessionTask(e.DB, session)
	if err != nil {
		return err
	}
	if _, err := model.FindResource(e.DB, "Pipeline", task.Run.PipelineName); err != nil {
		return fmt.Errorf("session pipeline unavailable: %w", err)
	}
	err = e.DB.Transaction(func(tx *gorm.DB) error {
		var number int
		if err := tx.Model(&model.AgentTurn{}).Where("run_id = ?", session.RunID).Select("COALESCE(MAX(number), 0)").Scan(&number).Error; err != nil {
			return err
		}
		turn := model.AgentTurn{RunID: session.RunID, SessionID: session.ID, Number: number + 1, Input: text, UserID: userID, Status: "queued"}
		if err := tx.Create(&turn).Error; err != nil {
			return err
		}
		if err := tx.Model(&session).Updates(map[string]any{"state": "queued", "current_turn_id": turn.ID, "last_error": ""}).Error; err != nil {
			return err
		}
		if err := tx.Model(&task.Run).Updates(map[string]any{"status": "queued", "error": "", "finished_at": nil}).Error; err != nil {
			return err
		}
		if err := tx.Model(&task.Event).Update("status", "waiting").Error; err != nil {
			return err
		}
		return tx.Create(&model.SessionEvent{RunID: session.RunID, TurnID: turn.ID, Type: "message", Role: "user", Status: "accepted", Text: text, ItemID: fmt.Sprintf("turn-input-%d", turn.ID)}).Error
	})
	if err == nil {
		e.notify()
	}
	return err
}

func (e *Engine) sessionTask(db *gorm.DB, session model.AgentSession) (Task, error) {
	task := Task{Encryption: e.Encryption}
	if err := db.First(&task.Run, session.RunID).Error; err != nil {
		return task, err
	}
	if err := db.First(&task.Event, task.Run.EventID).Error; err != nil {
		return task, err
	}
	var snapshot model.ExecutionSnapshot
	if err := json.Unmarshal(session.Snapshot, &snapshot); err != nil {
		return task, fmt.Errorf("read session resource snapshot: %w", err)
	}
	task.Pipeline, task.Worker, task.Workspace, task.Runner = snapshot.Pipeline, snapshot.Worker, snapshot.Workspace, snapshot.Runner
	return task, nil
}
