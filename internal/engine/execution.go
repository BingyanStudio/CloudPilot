package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/gorm"
)

func (e *Engine) execute(ctx context.Context, task Task, prepareErr error) {
	var artifacts []model.Artifact
	err := prepareErr
	if err == nil {
		artifacts, err = e.invoke(ctx, task)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	active := e.active[task.Run.ID]
	active.cancel()
	defer delete(e.active, task.Run.ID)
	status, message := "succeeded", ""
	if err != nil {
		status, message = "failed", err.Error()
	}
	if active.cancelled {
		status, message = "cancelled", "cancelled by operator"
	} else if errors.Is(err, context.Canceled) {
		message = "execution interrupted by scheduler shutdown"
	} else if errors.Is(err, context.DeadlineExceeded) {
		message = "execution exceeded pipeline timeout"
	}
	if persistErr := e.finish(task, status, message, artifacts, time.Now().UTC()); persistErr != nil {
		slog.Error("persist execution result failed", "run", task.Run.ID, "error", persistErr)
		// A failed artifact insert must not leave a completed execution occupying a slot.
		if fallbackErr := e.finish(task, "failed", "could not persist execution result", nil, time.Now().UTC()); fallbackErr != nil {
			slog.Error("persist execution failure failed", "run", task.Run.ID, "error", fallbackErr)
		}
	}
	e.notify()
}

func (e *Engine) invoke(ctx context.Context, task Task) (artifacts []model.Artifact, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("executor panic: %v", recovered)
		}
	}()
	return e.Executor.Execute(ctx, task, e.runLogger(task.Run.ID))
}

func (e *Engine) runLogger(runID uint) func(string, string) {
	var logMu sync.Mutex
	count, bytes := 0, 0
	return func(stream, message string) {
		logMu.Lock()
		defer logMu.Unlock()
		if count >= 10000 || bytes >= 10<<20 {
			return
		}
		if len(message) > 16<<10 {
			message = strings.ToValidUTF8(message[:16<<10], "") + " [truncated]"
		}
		count++
		bytes += len(message)
		entry := model.RunLog{RunID: runID, Stream: stream, Message: message}
		if err := e.DB.Create(&entry).Error; err != nil {
			slog.Error("persist run log failed", "run", runID, "error", err)
		}
	}
}

func (e *Engine) finish(task Task, status, message string, artifacts []model.Artifact, now time.Time) error {
	eventStatus := status
	availableAt := now
	if status == "failed" && task.Event.Attempts <= task.Pipeline.MaxRetries {
		eventStatus = "queued"
		availableAt = now.Add(retryDelay(task.Event.Attempts))
	}
	return e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Run{}).Where("id = ? AND status = ?", task.Run.ID, "running").Updates(map[string]any{"status": status, "error": message, "finished_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.Event{}).Where("id = ? AND status = ?", task.Event.ID, "running").Updates(map[string]any{"status": eventStatus, "available_at": availableAt}).Error; err != nil {
			return err
		}
		for _, artifact := range artifacts {
			artifact.ID, artifact.RunID = 0, task.Run.ID
			if err := tx.Create(&artifact).Error; err != nil {
				return err
			}
		}
		entry := model.RunLog{RunID: task.Run.ID, Stream: "system", Message: "Run " + status}
		if message != "" {
			entry.Message += ": " + message
		}
		return tx.Create(&entry).Error
	})
}

func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		return 5 * time.Minute
	}
	if attempt < 1 {
		attempt = 1
	}
	return time.Duration(1<<attempt) * time.Second
}

func (e *Engine) recover() error {
	now := time.Now().UTC()
	return e.DB.Transaction(func(tx *gorm.DB) error {
		if err := recoverSessions(tx, now); err != nil {
			return err
		}
		var runs []model.Run
		if err := tx.Where("status = ?", "running").Find(&runs).Error; err != nil {
			return err
		}
		for _, run := range runs {
			status := "failed"
			resource, err := model.FindResource(tx, "Pipeline", run.PipelineName)
			if err == nil {
				var spec model.PipelineSpec
				if resource.Decode(&spec) == nil && run.Attempt <= spec.MaxRetries {
					status = "queued"
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err = tx.Model(&run).Updates(map[string]any{"status": "failed", "error": "scheduler restarted before execution completed", "finished_at": now}).Error; err != nil {
				return err
			}
			if err = tx.Model(&model.Event{}).Where("id = ? AND status = ?", run.EventID, "running").Updates(map[string]any{"status": status, "available_at": now}).Error; err != nil {
				return err
			}
			if err = tx.Create(&model.RunLog{RunID: run.ID, Stream: "system", Message: "Interrupted run recovered after scheduler restart; event " + status}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&model.Event{}).Where("status = ?", "running").Update("status", "failed").Error
	})
}
