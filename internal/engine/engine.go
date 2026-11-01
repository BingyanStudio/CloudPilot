package engine

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/gorm"
)

// Executor provides an agent runtime independently of queue scheduling.
type Executor interface {
	Execute(context.Context, Task, func(stream, message string)) ([]model.Artifact, error)
}

// Task is the immutable resource configuration selected when an event starts.
type Task struct {
	Encryption cipher.AEAD `json:"-"`
	Run        model.Run
	Event      model.Event
	Pipeline   model.PipelineSpec
	Worker     model.WorkerSpec
	Workspace  model.WorkspaceSpec
	Runner     model.RunnerSpec
}

type execution struct {
	cancel    context.CancelFunc
	cancelled bool
	closing   bool
	sessionID uint
	controls  chan SessionControl
	pending   map[uint]bool
	failure   string
}

// Engine coordinates a single scheduler instance backed by persistent queue state.
type Engine struct {
	DB           *gorm.DB
	Executor     Executor
	Encryption   cipher.AEAD
	PollInterval time.Duration

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	active  map[uint]*execution
	closing map[uint]bool
	started bool
	stopped bool
	wg      sync.WaitGroup
	wake    chan struct{}
}

// Create configures a scheduler without starting background work.
func Create(db *gorm.DB, executor Executor) *Engine {
	return &Engine{DB: db, Executor: executor, active: map[uint]*execution{}, closing: map[uint]bool{}, wake: make(chan struct{}, 1)}
}

// Start recovers interrupted runs and starts dispatching eligible events.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return errors.New("scheduler already started")
	}
	if e.stopped {
		return errors.New("scheduler has stopped")
	}
	if e.DB == nil || e.Executor == nil {
		return errors.New("scheduler requires a database and executor")
	}
	if recovery, ok := e.Executor.(interface{ Recover(context.Context) error }); ok {
		if err := recovery.Recover(ctx); err != nil {
			return fmt.Errorf("recover runtime: %w", err)
		}
	}
	if err := e.recover(); err != nil {
		return fmt.Errorf("recover scheduler: %w", err)
	}
	e.ctx, e.cancel = context.WithCancel(ctx)
	e.started = true
	e.wg.Go(e.loop)
	return nil
}

// Stop cancels active executions and waits until their final state is persisted.
func (e *Engine) Stop(ctx context.Context) error {
	e.mu.Lock()
	e.stopped = true
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Trigger releases the pipeline's current backlog, while preserving its pause state.
func (e *Engine) Trigger(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := model.FindResource(e.DB, "Pipeline", name)
	if err != nil {
		return err
	}
	var spec model.PipelineSpec
	if err = r.Decode(&spec); err != nil {
		return err
	}
	if spec.Paused {
		return errors.New("pipeline is paused")
	}
	now := time.Now().UTC()
	state, err := e.pipelineState(r, now)
	if err != nil {
		return err
	}
	if err = e.DB.Model(&state).Update("drain_until", now).Error; err != nil {
		return err
	}
	e.notify()
	return nil
}

// Cancel requests cancellation of an execution owned by this scheduler.
func (e *Engine) Cancel(runID uint) error {
	e.mu.Lock()
	var session model.AgentSession
	if err := e.DB.Where("run_id = ?", runID).First(&session).Error; err == nil {
		e.mu.Unlock()
		return e.CloseSession(runID)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		e.mu.Unlock()
		return err
	}
	defer e.mu.Unlock()
	if active, ok := e.active[runID]; ok {
		active.cancelled = true
		active.cancel()
		return nil
	}
	var run model.Run
	if err := e.DB.First(&run, runID).Error; err != nil {
		return err
	}
	return fmt.Errorf("run is %s and cannot be cancelled", run.Status)
}

// Retry starts a fresh attempt budget for a failed or cancelled event.
func (e *Engine) Retry(eventID uint) (model.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var event model.Event
	err := e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&event, eventID).Error; err != nil {
			return err
		}
		if event.Status != "failed" && event.Status != "cancelled" && event.Status != "interrupted" {
			return errors.New("only failed, cancelled or interrupted events can be retried")
		}
		var open int64
		if err := tx.Model(&model.AgentSession{}).Joins("JOIN runs ON runs.id = agent_sessions.run_id").Where("runs.event_id = ? AND agent_sessions.state <> ?", eventID, "closed").Count(&open).Error; err != nil {
			return err
		}
		if open > 0 {
			return errors.New("close the existing interactive session before retrying as a new run")
		}
		event.Status, event.Attempts, event.AvailableAt = "queued", 0, time.Now().UTC()
		return tx.Save(&event).Error
	})
	if err == nil {
		e.notify()
	}
	return event, err
}

func (e *Engine) notify() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) loop() {
	interval := e.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := e.tick(time.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("scheduler dispatch failed", "error", err)
		}
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		case <-e.wake:
		}
	}
}

func (e *Engine) tick(now time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped || e.ctx.Err() != nil {
		return context.Canceled
	}
	var pipelines []model.Resource
	if err := e.DB.Where("kind = ?", "Pipeline").Order("id").Find(&pipelines).Error; err != nil {
		return err
	}
	var failures []error
	for _, pipeline := range pipelines {
		if err := e.dispatch(pipeline, now); err != nil {
			failures = append(failures, fmt.Errorf("pipeline %s: %w", pipeline.Name, err))
		}
	}
	return errors.Join(failures...)
}
