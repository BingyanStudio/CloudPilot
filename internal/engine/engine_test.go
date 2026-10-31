package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

type executorFunc func(context.Context, Task, func(string, string)) ([]model.Artifact, error)

func (f executorFunc) Execute(ctx context.Context, task Task, log func(string, string)) ([]model.Artifact, error) {
	return f(ctx, task, log)
}

func fixture(t *testing.T, spec model.PipelineSpec, executor Executor) (*gorm.DB, *Engine) {
	t.Helper()
	db, err := model.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	spec.WorkerRef, spec.WorkspaceRef, spec.RunnerRef = "worker", "workspace", "runner"
	for _, resource := range []struct {
		kind, name string
		spec       any
	}{
		{"Worker", "worker", model.WorkerSpec{Type: "codex"}},
		{"Workspace", "workspace", model.WorkspaceSpec{}},
		{"Runner", "runner", model.RunnerSpec{Type: "docker"}},
		{"Pipeline", "feedback", spec},
	} {
		data, err := json.Marshal(resource.spec)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.Create(&model.Resource{Kind: resource.kind, Name: resource.name, Spec: data, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	engine := Create(db, executor)
	engine.PollInterval = 10 * time.Millisecond
	engine.ctx, engine.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := engine.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return db, engine
}

func enqueue(t *testing.T, db *gorm.DB, created time.Time) model.Event {
	t.Helper()
	event := model.Event{PipelineName: "feedback", InputName: "feedback", Status: "queued", Payload: json.RawMessage(`{"message":"broken login"}`), AvailableAt: created, CreatedAt: created}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	return event
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func eventStatus(t *testing.T, db *gorm.DB, id uint) string {
	t.Helper()
	var event model.Event
	if err := db.First(&event, id).Error; err != nil {
		t.Fatal(err)
	}
	return event.Status
}

func receive(t *testing.T, started <-chan Task) Task {
	t.Helper()
	select {
	case task := <-started:
		return task
	case <-time.After(5 * time.Second):
		t.Fatal("execution did not start")
		return Task{}
	}
}

func TestScheduledBacklogKeepsCutoffAcrossRestart(t *testing.T) {
	var calls atomic.Int64
	db, engine := fixture(t, model.PipelineSpec{Schedule: "0 20 * * *", Timezone: "Asia/Shanghai", Concurrency: 2}, executorFunc(func(context.Context, Task, func(string, string)) ([]model.Artifact, error) {
		calls.Add(1)
		return nil, nil
	}))
	before := time.Date(2026, 10, 31, 11, 59, 0, 0, time.UTC)
	early := enqueue(t, db, before)
	late := enqueue(t, db, before.Add(2*time.Minute))
	if err := engine.tick(before); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("scheduled work started before the next release")
	}
	if err := engine.tick(before.Add(6 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return eventStatus(t, db, early.ID) == "succeeded" })
	if eventStatus(t, db, late.ID) != "queued" || calls.Load() != 1 {
		t.Fatal("event received after 20:00 entered the earlier batch")
	}
	// A fresh scheduler reads the durable release cutoff instead of opening the queue.
	restarted := Create(db, engine.Executor)
	restarted.ctx, restarted.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() { _ = restarted.Stop(context.Background()) })
	if err := restarted.tick(before.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if eventStatus(t, db, late.ID) != "queued" {
		t.Fatal("restart widened the scheduled batch")
	}
	if err := restarted.tick(before.Add(24*time.Hour + time.Minute)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return eventStatus(t, db, late.ID) == "succeeded" })
}

func TestConcurrencyCancellationAndManualRetry(t *testing.T) {
	started := make(chan Task, 4)
	release := make(chan struct{}, 4)
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 2, MaxRetries: 2}, executorFunc(func(ctx context.Context, task Task, log func(string, string)) ([]model.Artifact, error) {
		log("stdout", "working")
		started <- task
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return []model.Artifact{{Kind: "text", Name: "resolution", Content: "fixed"}}, nil
		}
	}))
	now := time.Now().UTC()
	for range 3 {
		enqueue(t, db, now)
	}
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, second := receive(t, started), receive(t, started)
	select {
	case <-started:
		t.Fatal("pipeline concurrency limit exceeded")
	case <-time.After(40 * time.Millisecond):
	}
	if err := engine.Cancel(first.Run.ID); err != nil {
		t.Fatal(err)
	}
	third := receive(t, started)
	eventually(t, func() bool { return eventStatus(t, db, first.Event.ID) == "cancelled" })
	release <- struct{}{}
	release <- struct{}{}
	eventually(t, func() bool {
		return eventStatus(t, db, second.Event.ID) == "succeeded" && eventStatus(t, db, third.Event.ID) == "succeeded"
	})
	if _, err := engine.Retry(second.Event.ID); err == nil {
		t.Fatal("successful event was retried")
	}
	if _, err := engine.Retry(first.Event.ID); err != nil {
		t.Fatal(err)
	}
	retried := receive(t, started)
	if retried.Event.ID != first.Event.ID || retried.Run.ID == first.Run.ID || retried.Run.Attempt != 1 {
		t.Fatalf("unexpected retry: %+v", retried.Run)
	}
	release <- struct{}{}
	eventually(t, func() bool { return eventStatus(t, db, first.Event.ID) == "succeeded" })
	var artifacts int64
	if err := db.Model(&model.Artifact{}).Count(&artifacts).Error; err != nil || artifacts != 3 {
		t.Fatalf("artifacts count %d: %v", artifacts, err)
	}
}

func TestRetryBackoffTimeoutAndRecovery(t *testing.T) {
	var calls atomic.Int64
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1, MaxRetries: 1}, executorFunc(func(context.Context, Task, func(string, string)) ([]model.Artifact, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("transient failure")
		}
		return nil, context.DeadlineExceeded
	}))
	now := time.Now().UTC()
	event := enqueue(t, db, now)
	if err := engine.tick(now); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		var persisted model.Event
		return db.First(&persisted, event.ID).Error == nil && persisted.Status == "queued" && persisted.Attempts == 1 && persisted.AvailableAt.After(now)
	})
	if err := engine.tick(now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("retry ignored backoff")
	}
	if err := engine.tick(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return eventStatus(t, db, event.ID) == "failed" })
	var latest model.Run
	if err := db.Order("id DESC").First(&latest).Error; err != nil || latest.Error != "execution exceeded pipeline timeout" || latest.Attempt != 2 {
		t.Fatalf("timeout or retry budget incorrect: %+v, %v", latest, err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		interrupted := enqueue(t, db, now)
		if err := db.Model(&interrupted).Updates(map[string]any{"status": "running", "attempts": attempt}).Error; err != nil {
			t.Fatal(err)
		}
		run := model.Run{EventID: interrupted.ID, PipelineName: "feedback", Status: "running", Attempt: attempt, StartedAt: now}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		if err := engine.recover(); err != nil {
			t.Fatal(err)
		}
		want := "queued"
		if attempt == 2 {
			want = "failed"
		}
		if got := eventStatus(t, db, interrupted.ID); got != want {
			t.Fatalf("recovery attempt %d: got %s, want %s", attempt, got, want)
		}
	}
}

func TestLatestScheduleAcrossLongOutageAndDST(t *testing.T) {
	for _, tc := range []struct{ expression, after, now, want string }{
		{"CRON_TZ=Asia/Shanghai 0 20 * * *", "2020-01-01T00:00:00Z", "2026-10-31T13:17:23Z", "2026-10-31T12:00:00Z"},
		{"CRON_TZ=UTC * * * * *", "2020-01-01T00:00:00Z", "2026-10-31T13:17:23Z", "2026-10-31T13:17:00Z"},
		{"CRON_TZ=America/New_York 30 1 * * *", "2026-10-31T00:00:00Z", "2026-11-01T07:00:00Z", "2026-11-01T06:30:00Z"},
	} {
		schedule, err := cron.ParseStandard(tc.expression)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := time.Parse(time.RFC3339, tc.after)
		now, _ := time.Parse(time.RFC3339, tc.now)
		got := latestSchedule(schedule, after, now)
		if got == nil || got.Format(time.RFC3339) != tc.want {
			t.Fatalf("%s: got %v, want %s", tc.expression, got, tc.want)
		}
	}
}

type recoveryExecutor struct {
	err error
}

func (f *recoveryExecutor) Execute(context.Context, Task, func(string, string)) ([]model.Artifact, error) {
	return nil, nil
}

func (f *recoveryExecutor) Recover(context.Context) error { return f.err }

func TestRuntimeRecoveryPrecedesRequeue(t *testing.T) {
	executor := &recoveryExecutor{err: errors.New("docker unavailable")}
	db, engine := fixture(t, model.PipelineSpec{Paused: true, MaxRetries: 1}, executor)
	if err := engine.Start(context.Background()); !errors.Is(err, executor.err) {
		t.Fatalf("runtime cleanup must run even without interrupted database runs: %v", err)
	}
	event := enqueue(t, db, time.Now().UTC())
	if err := db.Model(&event).Updates(map[string]any{"status": "running", "attempts": 1}).Error; err != nil {
		t.Fatal(err)
	}
	run := model.Run{EventID: event.ID, PipelineName: "feedback", Status: "running", Attempt: 1}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(context.Background()); !errors.Is(err, executor.err) {
		t.Fatalf("expected runtime recovery failure, got %v", err)
	}
	if got := eventStatus(t, db, event.ID); got != "running" {
		t.Fatal("event was requeued before its orphaned runtime was stopped")
	}
	executor.err = nil
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := eventStatus(t, db, event.ID); got != "queued" {
		t.Fatalf("recovered event status %s", got)
	}
}

func TestExecutionTimeoutCancelsExecutor(t *testing.T) {
	db, engine := fixture(t, model.PipelineSpec{Concurrency: 1, TimeoutSeconds: 1}, executorFunc(func(ctx context.Context, _ Task, _ func(string, string)) ([]model.Artifact, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	event := enqueue(t, db, time.Now().UTC())
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return eventStatus(t, db, event.ID) == "failed" })
	var run model.Run
	if err := db.Where("event_id = ?", event.ID).First(&run).Error; err != nil || run.Error != "execution exceeded pipeline timeout" {
		t.Fatalf("unexpected timed-out run: %+v, %v", run, err)
	}
}
