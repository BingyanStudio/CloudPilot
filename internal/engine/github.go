package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/github"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/gorm"
)

const sourceCheckInterval = 30 * time.Second
const sourceStopped = "GitHub issue is closed or no longer matches input filters"

var errSourceCheckBudget = errors.New("GitHub check budget exhausted for this scheduling cycle")

// RefreshGitHub schedules a fresh API read after an authenticated issue delivery.
func (e *Engine) RefreshGitHub(eventID uint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, active := range e.active {
		if active.eventID == eventID {
			active.sourceCheckAt = time.Time{}
		}
	}
	err := e.DB.Model(&model.Event{}).Where("id = ? AND status IN ?", eventID, []string{"queued", "waiting"}).Update("available_at", time.Now().UTC()).Error
	if err == nil {
		e.notify()
	}
	return err
}

// checkGitHub uses the trusted acknowledgement association, never a payload source hint.
// The caller holds mu; network I/O releases it so controls and progress remain responsive.
func (e *Engine) checkGitHub(event model.Event) (bool, error) {
	var source model.GitHubIssueNotification
	if err := e.DB.Where("event_id = ?", event.ID).First(&source).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return true, nil
		}
		return false, err
	}
	var original struct {
		Issue struct{ ID int64 } `json:"issue"`
	}
	if json.Unmarshal(event.Payload, &original) != nil || original.Issue.ID < 1 {
		return false, errors.New("GitHub event issue identity is unavailable")
	}
	input, err := model.FindResource(e.DB, "Input", event.InputName)
	if err != nil {
		return false, fmt.Errorf("read GitHub input: %w", err)
	}
	var spec model.InputSpec
	if err := input.Decode(&spec); err != nil {
		return false, err
	}
	if spec.Type != "github_issue" || spec.GitHub == nil || !strings.EqualFold(spec.GitHub.Repository, source.Repository) {
		return false, nil
	}
	token, err := model.ResolveSecret(e.Encryption, "GitHub", spec.GitHub.Token, spec.GitHub.TokenSecretRef)
	if err != nil {
		return false, errors.New("GitHub token is unavailable")
	}
	if e.sourceChecksLeft == 0 {
		return false, errSourceCheckBudget
	}
	e.sourceChecksLeft--
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	e.mu.Unlock()
	issue, err := github.FetchIssue(ctx, e.GitHubClient, source.Repository, source.IssueNumber, token)
	cancel()
	e.mu.Lock()
	if e.stopped || e.ctx.Err() != nil {
		return false, context.Canceled
	}
	if err != nil {
		return false, err
	}
	if issue.ID != original.Issue.ID {
		return false, errors.New("GitHub issue identity has changed")
	}
	current, err := model.FindResource(e.DB, "Input", event.InputName)
	if err != nil || current.Generation != input.Generation {
		return false, errors.New("GitHub input changed during eligibility check")
	}
	return issue.Matches(*spec.GitHub), nil
}

// deferGitHubCheck leaves the attempt budget untouched and lets later candidates advance.
func (e *Engine) deferGitHubCheck(event model.Event, err error) error {
	slog.Warn("GitHub eligibility check delayed", "event", event.ID, "error", err)
	return e.DB.Model(&event).Where("status IN ?", []string{"queued", "waiting"}).Update("available_at", time.Now().UTC().Add(sourceCheckInterval)).Error
}

// reconcileGitHub stops active work whose current issue no longer qualifies.
func (e *Engine) reconcileGitHub(now time.Time) error {
	ids := make([]uint, 0, len(e.active))
	for id := range e.active {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uint) int { return e.active[a].sourceCheckAt.Compare(e.active[b].sourceCheckAt) })
	for _, runID := range ids {
		if e.sourceChecksLeft <= 2 {
			break
		}
		active := e.active[runID]
		if active == nil {
			continue
		}
		if active.sourceStopped != "" || active.closing || now.Before(active.sourceCheckAt) {
			continue
		}
		active.sourceCheckAt = now.Add(sourceCheckInterval)
		var event model.Event
		if err := e.DB.First(&event, active.eventID).Error; err != nil {
			return err
		}
		eligible, err := e.checkGitHub(event)
		if e.active[runID] != active || active.closing {
			continue
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			slog.Warn("GitHub eligibility refresh failed; keeping execution", "run", runID, "error", err)
			continue
		}
		if !eligible {
			active.sourceStopped = sourceStopped
			active.cancel()
		}
	}
	return nil
}

// canDispatch rechecks controls that may change while the scheduler reads GitHub.
func (e *Engine) canDispatch(pipeline string) (bool, error) {
	resource, err := model.FindResource(e.DB, "Pipeline", pipeline)
	if err != nil {
		return false, err
	}
	var spec model.PipelineSpec
	if err := resource.Decode(&spec); err != nil {
		return false, err
	}
	var running int64
	if err := e.DB.Model(&model.Run{}).Where("pipeline_name = ? AND status = ?", pipeline, "running").Count(&running).Error; err != nil {
		return false, err
	}
	return !spec.Paused && running < int64(max(spec.Concurrency, 1)), nil
}

// rejectGitHubTurn preserves an idle workspace when a queued follow-up loses eligibility.
func (e *Engine) rejectGitHubTurn(task Task, session model.AgentSession, turn model.AgentTurn, now time.Time) error {
	return e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&turn).Where("status = ?", "queued").Updates(map[string]any{"status": "interrupted", "finished_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&session).Where("state = ?", "queued").Updates(map[string]any{"state": "interrupted", "last_error": sourceStopped}).Error; err != nil {
			return err
		}
		if err := tx.Model(&task.Run).Where("status = ?", "queued").Updates(map[string]any{"status": "interrupted", "error": sourceStopped, "finished_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&task.Event).Where("status = ?", "waiting").Update("status", "interrupted").Error; err != nil {
			return err
		}
		return tx.Create(&model.SessionEvent{RunID: task.Run.ID, TurnID: turn.ID, Type: "status", Status: "interrupted", Text: sourceStopped}).Error
	})
}
