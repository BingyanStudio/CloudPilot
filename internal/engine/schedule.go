package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

func (e *Engine) pipelineState(resource model.Resource, now time.Time) (model.PipelineState, error) {
	var state model.PipelineState
	err := e.DB.Where("name = ?", resource.Name).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		checkpoint := resource.CreatedAt
		if checkpoint.IsZero() || checkpoint.After(now) {
			checkpoint = now
		}
		state = model.PipelineState{Name: resource.Name, LastScheduledAt: &checkpoint}
		err = e.DB.Create(&state).Error
	}
	return state, err
}

func (e *Engine) cutoff(resource model.Resource, spec model.PipelineSpec, now time.Time) (*time.Time, error) {
	if spec.Schedule == "" {
		return &now, nil
	}
	tz := spec.Timezone
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	schedule, err := cron.ParseStandard("CRON_TZ=" + tz + " " + spec.Schedule)
	if err != nil {
		return nil, err
	}
	state, err := e.pipelineState(resource, now)
	if err != nil {
		return nil, err
	}
	if state.LastScheduledAt == nil {
		state.LastScheduledAt = &now
	}
	if scheduled := latestSchedule(schedule, *state.LastScheduledAt, now); scheduled != nil {
		state.LastScheduledAt = scheduled
		if state.DrainUntil == nil || state.DrainUntil.Before(*scheduled) {
			state.DrainUntil = scheduled
		}
		if err = e.DB.Save(&state).Error; err != nil {
			return nil, err
		}
	}
	return state.DrainUntil, nil
}

// latestSchedule finds the latest missed tick without replaying every missed minute.
func latestSchedule(schedule cron.Schedule, after, now time.Time) *time.Time {
	first := schedule.Next(after)
	if first.IsZero() || first.After(now) {
		return nil
	}
	low, high := first.Unix(), now.Unix()
	for low < high {
		mid := low + (high-low+1)/2
		next := schedule.Next(time.Unix(mid-1, 0))
		if !next.IsZero() && !next.After(now) {
			low = mid
		} else {
			high = mid - 1
		}
	}
	result := time.Unix(low, 0).UTC()
	return &result
}

func (e *Engine) dispatch(resource model.Resource, now time.Time) error {
	var spec model.PipelineSpec
	if err := resource.Decode(&spec); err != nil {
		return err
	}
	if spec.Paused {
		return nil
	}
	if spec.Concurrency < 1 {
		spec.Concurrency = 1
	}
	if spec.TimeoutSeconds < 1 {
		spec.TimeoutSeconds = 3600
	}
	var running int64
	if err := e.DB.Model(&model.Run{}).Where("pipeline_name = ? AND status = ?", resource.Name, "running").Count(&running).Error; err != nil {
		return err
	}
	running, err := e.dispatchTurns(resource.Name, spec.Concurrency, running, now)
	if err != nil {
		return err
	}
	cutoff, err := e.cutoff(resource, spec, now)
	if err != nil || cutoff == nil {
		return err
	}
	for running < int64(spec.Concurrency) {
		var task Task
		task.Pipeline = spec
		if err := e.DB.Where("pipeline_name = ? AND status = ? AND available_at <= ? AND created_at <= ?", resource.Name, "queued", now, *cutoff).Order("available_at, id").First(&task.Event).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				break
			}
			return err
		}
		eligible, checkErr := e.checkGitHub(task.Event)
		if errors.Is(checkErr, errSourceCheckBudget) {
			break
		}
		if checkErr != nil {
			if errors.Is(checkErr, context.Canceled) {
				return checkErr
			}
			if err := e.deferGitHubCheck(task.Event, checkErr); err != nil {
				return err
			}
			continue
		}
		if !eligible {
			if err := e.DB.Model(&task.Event).Where("status = ?", "queued").Update("status", "cancelled").Error; err != nil {
				return err
			}
			slog.Info("queued GitHub event cancelled", "event", task.Event.ID, "reason", sourceStopped)
			continue
		}
		allowed, err := e.canDispatch(resource.Name)
		if err != nil || !allowed {
			return err
		}
		err = e.DB.Transaction(func(tx *gorm.DB) error {
			result := tx.Model(&model.Event{}).Where("id = ? AND status = ?", task.Event.ID, "queued").Updates(map[string]any{"status": "running", "attempts": gorm.Expr("attempts + 1")})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("event was claimed by another scheduler")
			}
			task.Event.Attempts++
			task.Event.Status = "running"
			task.Run = model.Run{EventID: task.Event.ID, PipelineName: resource.Name, Status: "running", Attempt: task.Event.Attempts, WorkerName: spec.WorkerRef, WorkspaceName: spec.WorkspaceRef, RunnerName: spec.RunnerRef, StartedAt: now}
			return tx.Create(&task.Run).Error
		})
		if errors.Is(err, gorm.ErrRecordNotFound) {
			break
		}
		if err != nil {
			return err
		}
		prepareErr := e.prepare(&task)
		if executor, ok := e.Executor.(InteractiveExecutor); prepareErr == nil && ok && executor.SupportsInteraction(task) {
			if err := e.startSession(task, now); err == nil {
				running++
				continue
			} else {
				prepareErr = err
			}
		}
		ctx, cancel := context.WithTimeout(e.ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
		e.active[task.Run.ID] = &execution{cancel: cancel, eventID: task.Event.ID, sourceCheckAt: now.Add(sourceCheckInterval)}
		e.wg.Go(func() { e.execute(ctx, task, prepareErr) })
		running++
	}
	return nil
}

func (e *Engine) prepare(task *Task) error {
	task.Encryption = e.Encryption
	for _, ref := range []struct {
		kind, name string
		target     any
	}{
		{"Worker", task.Pipeline.WorkerRef, &task.Worker},
		{"Workspace", task.Pipeline.WorkspaceRef, &task.Workspace},
		{"Runner", task.Pipeline.RunnerRef, &task.Runner},
	} {
		resource, err := model.FindResource(e.DB, ref.kind, ref.name)
		if err != nil {
			return fmt.Errorf("resolve %s %q: %w", ref.kind, ref.name, err)
		}
		if err = resource.Decode(ref.target); err != nil {
			return err
		}
	}
	return nil
}
