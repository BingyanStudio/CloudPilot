package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/gorm"
)

type githubTransport func(*http.Request) (*http.Response, error)

func (f githubTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func githubEvent(t *testing.T, db *gorm.DB, now time.Time) model.Event {
	t.Helper()
	t.Setenv("CLOUDPILOT_TEST_GITHUB_TOKEN", "source-token")
	input := model.Resource{Kind: "Input", Name: "feedback", Spec: json.RawMessage(`{"type":"github_issue","github":{"repository":"org/repo","tokenSecretRef":"CLOUDPILOT_TEST_GITHUB_TOKEN","labels":["ready"],"assignees":["agent"]}}`)}
	if err := db.Create(&input).Error; err != nil {
		t.Fatal(err)
	}
	event := enqueue(t, db, now)
	event.Payload = json.RawMessage(`{"source":"github_issue","issue":{"id":42,"number":7}}`)
	if err := db.Save(&event).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.GitHubIssueNotification{EventID: event.ID, Repository: "org/repo", IssueNumber: 7}).Error; err != nil {
		t.Fatal(err)
	}
	return event
}

func githubClient(t *testing.T, response func() (int, string)) *http.Client {
	t.Helper()
	return &http.Client{Transport: githubTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.github.com/repos/org/repo/issues/7" || req.Header.Get("Authorization") != "Bearer source-token" {
			t.Errorf("unexpected issue request: %s", req.URL)
		}
		if _, ok := req.Context().Deadline(); !ok {
			t.Error("issue request has no timeout")
		}
		code, body := response()
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
}

func TestGitHubPreflightDoesNotSpendAttemptsOrBlockLaterEvents(t *testing.T) {
	for _, tc := range []struct {
		name, body, status string
		code               int
	}{
		{"closed", `{"id":42,"number":7,"state":"closed","labels":[{"name":"ready"}]}`, "cancelled", 200},
		{"unmatched", `{"id":42,"number":7,"state":"open","labels":[],"assignees":[]}`, "cancelled", 200},
		{"forbidden", `{}`, "queued", 403},
		{"inaccessible", `{}`, "queued", 404},
		{"unavailable", `{}`, "queued", 503},
		{"malformed", `{"state":"closed"}`, "queued", 200},
		{"changed identity", `{"id":99,"number":7,"state":"closed"}`, "queued", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan Task, 2)
			db, engine := fixture(t, model.PipelineSpec{Concurrency: 1}, executorFunc(func(_ context.Context, task Task, _ func(string, string)) ([]model.Artifact, error) {
				started <- task
				return nil, nil
			}))
			now := time.Now().UTC()
			event := githubEvent(t, db, now)
			ordinary := enqueue(t, db, now)
			if err := db.Model(&ordinary).Update("payload", json.RawMessage(`{"source":"github_issue","issue":{"id":42,"number":7}}`)).Error; err != nil {
				t.Fatal(err)
			}
			engine.GitHubClient = githubClient(t, func() (int, string) { return tc.code, tc.body })
			if err := engine.tick(now); err != nil {
				t.Fatal(err)
			}
			if task := receive(t, started); task.Event.ID != ordinary.ID {
				t.Fatalf("ineligible issue started: %+v", task.Event)
			}
			if err := db.First(&event, event.ID).Error; err != nil {
				t.Fatal(err)
			}
			if event.Status != tc.status || event.Attempts != 0 || (tc.status == "queued" && !event.AvailableAt.After(now)) {
				t.Fatalf("unexpected preflight result: %+v", event)
			}
		})
	}
}

func TestGitHubReconciliationRetainsSessionAndChecksFollowups(t *testing.T) {
	var closed, unavailable atomic.Bool
	var cleanups atomic.Int32
	executor := &interactiveExecutor{started: make(chan *interactiveCall, 4), close: func() error { cleanups.Add(1); return nil }}
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1, TimeoutSeconds: 300}, executor)
	event := githubEvent(t, db, time.Now().UTC())
	engine.GitHubClient = githubClient(t, func() (int, string) {
		if unavailable.Load() {
			return 503, `{}`
		}
		state := "open"
		if closed.Load() {
			state = "closed"
		}
		return 200, fmt.Sprintf(`{"id":42,"number":7,"state":%q,"assignees":[{"login":"AGENT"}]}`, state)
	})
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := nextTurn(t, executor)
	unavailable.Store(true)
	if err := engine.tick(time.Now().UTC().Add(sourceCheckInterval)); err != nil {
		t.Fatal(err)
	}
	if eventStatus(t, db, event.ID) != "running" {
		t.Fatal("API outage interrupted an active issue")
	}
	unavailable.Store(false)
	closed.Store(true)
	if err := engine.RefreshGitHub(event.ID); err != nil {
		t.Fatal(err)
	}
	session := waitSession(t, db, first.Task.Run.ID, "interrupted")
	if session.LastError != sourceStopped || cleanups.Load() != 0 {
		t.Fatalf("source stop lost its reason or cleaned workspace: %+v", session)
	}
	if err := engine.Resume(first.Task.Run.ID, "continue", 1); err != nil {
		t.Fatal(err)
	}
	waitSession(t, db, first.Task.Run.ID, "interrupted")
	var followup model.AgentTurn
	if err := db.Where("run_id = ? AND number = 2", first.Task.Run.ID).First(&followup).Error; err != nil || followup.Status != "interrupted" || len(executor.started) != 0 {
		t.Fatalf("closed issue follow-up started: %+v, %v", followup, err)
	}
	closed.Store(false)
	if err := engine.Resume(first.Task.Run.ID, "continue", 1); err != nil {
		t.Fatal(err)
	}
	resumed := nextTurn(t, executor)
	if resumed.Session.ID != session.ID || resumed.Session.ThreadID != session.ThreadID {
		t.Fatal("eligible follow-up did not retain the session")
	}
	closed.Store(true)
	if err := engine.tick(time.Now().UTC().Add(sourceCheckInterval)); err != nil {
		t.Fatal(err)
	}
	waitSession(t, db, first.Task.Run.ID, "interrupted")
	if cleanups.Load() != 0 {
		t.Fatal("periodic reconciliation removed the workspace")
	}
}

func TestGitHubPreflightDoesNotHoldControlLock(t *testing.T) {
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1}, executorFunc(func(context.Context, Task, func(string, string)) ([]model.Artifact, error) {
		t.Error("worker launched before eligibility was known")
		return nil, nil
	}))
	event := githubEvent(t, db, time.Now().UTC())
	checking := make(chan struct{})
	engine.GitHubClient = &http.Client{Transport: githubTransport(func(req *http.Request) (*http.Response, error) {
		close(checking)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-checking:
	case <-time.After(5 * time.Second):
		t.Fatal("eligibility check did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := engine.Stop(ctx); err != nil {
		t.Fatalf("network check blocked shutdown: %v", err)
	}
	if err := db.First(&event, event.ID).Error; err != nil || event.Attempts != 0 {
		t.Fatalf("shutdown spent an attempt: %+v, %v", event, err)
	}
}

func TestGitHubCheckBudgetLetsTheBacklogAdvance(t *testing.T) {
	started := make(chan Task, 1)
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1}, executorFunc(func(_ context.Context, task Task, _ func(string, string)) ([]model.Artifact, error) {
		started <- task
		return nil, nil
	}))
	now := time.Now().UTC()
	first := githubEvent(t, db, now)
	for range 4 {
		event := enqueue(t, db, now)
		if err := db.Model(&event).Update("payload", first.Payload).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.GitHubIssueNotification{EventID: event.ID, Repository: "org/repo", IssueNumber: 7}).Error; err != nil {
			t.Fatal(err)
		}
	}
	ordinary := enqueue(t, db, now)
	calls := 0
	engine.GitHubClient = githubClient(t, func() (int, string) { calls++; return 503, `{}` })
	if err := engine.tick(now); err != nil || calls != 4 {
		t.Fatalf("unbounded API requests: %d, %v", calls, err)
	}
	if err := engine.tick(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if task := receive(t, started); task.Event.ID != ordinary.ID || calls != 5 {
		t.Fatalf("deferred checks blocked later work: event %d, requests %d", task.Event.ID, calls)
	}
}

func TestGitHubPreflightRechecksPipelinePause(t *testing.T) {
	started := make(chan Task, 1)
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1}, executorFunc(func(_ context.Context, task Task, _ func(string, string)) ([]model.Artifact, error) {
		started <- task
		return nil, nil
	}))
	event := githubEvent(t, db, time.Now().UTC())
	checking, release := make(chan struct{}), make(chan struct{})
	engine.GitHubClient = githubClient(t, func() (int, string) {
		close(checking)
		<-release
		return 200, `{"id":42,"number":7,"state":"open","labels":[{"name":"ready"}]}`
	})
	if err := engine.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-checking:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("eligibility check did not start")
	}
	updatePipeline(t, db, true)
	close(release)
	if err := engine.tick(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&event, event.ID).Error; err != nil || event.Attempts != 0 || len(started) != 0 {
		t.Fatalf("pipeline pause was ignored after preflight: %+v, %v", event, err)
	}
}
