package views_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/controller"
	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/views"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

type interactiveFixture struct {
	started chan model.AgentTurn
	finish  chan struct{}
	closed  atomic.Int32
}

func (*interactiveFixture) Execute(context.Context, engine.Task, func(string, string)) ([]model.Artifact, error) {
	return nil, errors.New("interactive worker used batch execution")
}

func (*interactiveFixture) SupportsInteraction(engine.Task) bool { return true }

func (f *interactiveFixture) ExecuteTurn(ctx context.Context, _ engine.Task, _ model.AgentSession, turn model.AgentTurn, controls <-chan engine.SessionControl, emit func(engine.SessionUpdate), _ func(string, string)) (engine.TurnResult, error) {
	providerID := fmt.Sprintf("turn-%d", turn.Number)
	emit(engine.SessionUpdate{ThreadID: "thread-fixture", ProviderTurnID: providerID})
	emit(engine.SessionUpdate{Event: &model.SessionEvent{Type: "message", ItemID: "answer", Role: "assistant", Status: "streaming", Text: "Inspecting the feedback"}})
	emit(engine.SessionUpdate{Event: &model.SessionEvent{Type: "tool", ItemID: "command", Tool: "commandExecution", Command: "git status --short", Status: "inProgress"}})
	f.started <- turn
	result := engine.TurnResult{ThreadID: "thread-fixture", ProviderTurnID: providerID}
	for {
		select {
		case control := <-controls:
			if control.Type == "interrupt" {
				result.Interrupted = true
				return result, nil
			}
			emit(engine.SessionUpdate{ControlID: control.ID})
		case <-f.finish:
			emit(engine.SessionUpdate{Event: &model.SessionEvent{Type: "tool", ItemID: "command", Tool: "commandExecution", Command: "git status --short", Status: "completed", Output: "working tree clean"}})
			emit(engine.SessionUpdate{Event: &model.SessionEvent{Type: "message", ItemID: "answer", Role: "assistant", Status: "completed", Text: "Feedback checked"}})
			return result, nil
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
}

func (f *interactiveFixture) CloseSession(context.Context, engine.Task, model.AgentSession) error {
	f.closed.Add(1)
	return nil
}

func TestInteractiveSessionAPI(t *testing.T) {
	dir := t.TempDir()
	db, err := model.Open("sqlite", filepath.Join(dir, "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := model.BootstrapAdmin(db, "admin", "test-password-123"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		kind, name string
		spec       any
	}{
		{"Worker", "codex", model.WorkerSpec{Type: "codex", Model: "fixture", APIKeySecretRef: "MODEL_KEY"}},
		{"Workspace", "app", model.WorkspaceSpec{}},
		{"Runner", "docker", model.RunnerSpec{Type: "docker"}},
		{"Pipeline", "feedback", model.PipelineSpec{WorkerRef: "codex", WorkspaceRef: "app", RunnerRef: "docker", Concurrency: 1, TimeoutSeconds: 30}},
	} {
		spec, _ := json.Marshal(item.spec)
		if err := db.Create(&model.Resource{Kind: item.kind, Name: item.name, Spec: spec}).Error; err != nil {
			t.Fatal(err)
		}
	}
	fixture := &interactiveFixture{started: make(chan model.AgentTurn, 4), finish: make(chan struct{}, 4)}
	scheduler := engine.Create(db, fixture)
	scheduler.PollInterval = 5 * time.Millisecond
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	e := echo.New()
	views.Register(e, &controller.App{DB: db, Engine: scheduler, DataDir: dir}, filepath.Join(dir, "frontend"))
	var cookie *http.Cookie
	request := func(method, path, body string, authenticated bool, origin string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authenticated && cookie != nil {
			req.AddCookie(cookie)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		response := httptest.NewRecorder()
		e.ServeHTTP(response, req)
		return response
	}
	login := request(http.MethodPost, "/api/auth/login", `{"username":"admin","password":"test-password-123"}`, false, "")
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	cookie = login.Result().Cookies()[0]
	now := time.Now().UTC()
	event := model.Event{PipelineName: "feedback", InputName: "feedback", Payload: json.RawMessage(`{"message":"fixture feedback"}`), Status: "queued", AvailableAt: now}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	var first model.AgentTurn
	select {
	case first = <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("initial turn did not start")
	}
	base := fmt.Sprintf("/api/runs/%d", first.RunID)
	for _, path := range []string{"/messages", "/interrupt", "/resume", "/close"} {
		if response := request(http.MethodPost, base+path, `{}`, false, ""); response.Code != http.StatusUnauthorized {
			t.Fatalf("unguarded control %s: %d", path, response.Code)
		}
		if response := request(http.MethodPost, base+path, `{}`, true, "https://attacker.test"); response.Code != http.StatusForbidden {
			t.Fatalf("cross-origin control %s: %d", path, response.Code)
		}
	}
	if response := request(http.MethodGet, base+"/timeline", "", false, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unguarded timeline: %d", response.Code)
	}
	for _, body := range []string{`{}`, `{"message":"  "}`, `{"message":42}`, `{"message":"ok","unknown":true}`, `null`, `{"message":"\u0000"}`, `{"message":"ok"}{}`} {
		if response := request(http.MethodPost, base+"/messages", body, true, ""); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid message accepted %s: %d", body, response.Code)
		}
	}
	tooLong, _ := json.Marshal(map[string]string{"message": strings.Repeat("界", 11000)})
	if response := request(http.MethodPost, base+"/messages", string(tooLong), true, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("oversized UTF-8 message accepted: %d", response.Code)
	}
	for _, after := range []string{"-1", "invalid", "18446744073709551615"} {
		if response := request(http.MethodGet, base+"/timeline?after="+after, "", true, ""); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid timeline cursor %s: %d", after, response.Code)
		}
	}
	for _, path := range []string{"/timeline", ""} {
		if response := request(http.MethodGet, "/api/runs/999999"+path, "", true, ""); response.Code != http.StatusNotFound {
			t.Fatalf("missing run %s: %d", path, response.Code)
		}
	}
	response := request(http.MethodPost, base+"/messages", `{"message":"Only fix the Android client"}`, true, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("send supplement: %d %s", response.Code, response.Body)
	}
	awaitSession(t, func() bool {
		var count int64
		db.Model(&model.SessionEvent{}).Where("run_id = ? AND type = ? AND status = ?", first.RunID, "control", "accepted").Count(&count)
		return count == 1
	})
	response = request(http.MethodPost, base+"/interrupt", "", true, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("interrupt: %d %s", response.Code, response.Body)
	}
	awaitSession(t, func() bool {
		var session model.AgentSession
		return db.Where("run_id = ?", first.RunID).First(&session).Error == nil && session.State == "interrupted"
	})
	response = request(http.MethodGet, base, "", true, "")
	var detail struct {
		Run          model.Run           `json:"run"`
		Session      *model.AgentSession `json:"session"`
		Capabilities engine.Capabilities `json:"capabilities"`
		Turns        []model.AgentTurn   `json:"turns"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &detail) != nil || detail.Session == nil || !detail.Capabilities.CanResume || len(detail.Turns) != 1 {
		t.Fatalf("interactive detail missing session/capabilities/turns: %d %s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), "snapshot") || strings.Contains(response.Body.String(), "MODEL_KEY") {
		t.Fatal("internal session configuration exposed by run detail")
	}
	response = request(http.MethodPost, base+"/resume", `{"message":"Continue with the extra information"}`, true, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", response.Code, response.Body)
	}
	var second model.AgentTurn
	select {
	case second = <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("continuation did not start")
	}
	if second.RunID != first.RunID || second.SessionID != first.SessionID || second.Number != first.Number+1 || second.Input != "Continue with the extra information" || second.UserID == 0 {
		t.Fatalf("continuation lost session or actor: first=%+v second=%+v", first, second)
	}
	fixture.finish <- struct{}{}
	awaitSession(t, func() bool {
		var session model.AgentSession
		return db.Where("run_id = ?", first.RunID).First(&session).Error == nil && session.State == "idle"
	})
	response = request(http.MethodGet, base+"/timeline", "", true, "")
	var events []model.SessionEvent
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &events) != nil || len(events) == 0 {
		t.Fatalf("timeline: %d %s", response.Code, response.Body)
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID || events[i].RunID != first.RunID {
			t.Fatal("timeline was not scoped and ordered")
		}
	}
	response = request(http.MethodGet, fmt.Sprintf("%s/timeline?after=%d", base, events[len(events)-1].ID), "", true, "")
	var next []model.SessionEvent
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &next) != nil || next == nil || len(next) != 0 {
		t.Fatalf("timeline cursor replayed events: %d %s", response.Code, response.Body)
	}
	response = request(http.MethodPost, base+"/close", "", true, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("close: %d %s", response.Code, response.Body)
	}
	awaitSession(t, func() bool {
		var session model.AgentSession
		return db.Where("run_id = ?", first.RunID).First(&session).Error == nil && session.State == "closed"
	})
	if fixture.closed.Load() != 1 {
		t.Fatalf("session runtime cleanup count %d", fixture.closed.Load())
	}
	if response := request(http.MethodPost, base+"/resume", "", true, ""); response.Code != http.StatusConflict {
		t.Fatalf("closed session accepted resume: %d %s", response.Code, response.Body)
	}
}

func awaitSession(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("session did not reach expected state")
}

func TestRunDetailReadsConsistentCompletionSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	db, err := model.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := db.DB()
	t.Cleanup(func() { _ = reader.Close() })
	writer, err := model.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	writerSQL, _ := writer.DB()
	t.Cleanup(func() { _ = writerSQL.Close() })
	event := model.Event{Status: "running", Payload: json.RawMessage(`{}`)}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	run := model.Run{EventID: event.ID, Status: "running"}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	session := model.AgentSession{RunID: run.ID, State: "running"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	turn := model.AgentTurn{RunID: run.ID, SessionID: session.ID, Number: 1, Status: "running"}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatal(err)
	}
	var completed atomic.Bool
	var completionErr error
	if err := db.Callback().Query().After("gorm:query").Register("test:finish-during-run-read", func(tx *gorm.DB) {
		if tx.Statement.Table != "artifacts" || !completed.CompareAndSwap(false, true) {
			return
		}
		completionErr = writer.Transaction(func(tx *gorm.DB) error {
			for _, update := range []struct {
				record        any
				column, value string
			}{{&run, "status", "succeeded"}, {&event, "status", "succeeded"}, {&session, "state", "idle"}, {&turn, "status", "completed"}} {
				if err := tx.Model(update.record).Update(update.column, update.value).Error; err != nil {
					return err
				}
			}
			return tx.Create(&model.Artifact{RunID: run.ID, Kind: "text", Name: "result", Content: "completed artifact"}).Error
		})
	}); err != nil {
		t.Fatal(err)
	}
	app := &controller.App{DB: db}
	e := echo.New()
	e.GET("/runs/:id", app.Run)
	read := func() (model.Run, model.Event, *model.AgentSession, []model.AgentTurn, []model.Artifact) {
		t.Helper()
		response := httptest.NewRecorder()
		e.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/runs/%d", run.ID), nil))
		var detail struct {
			Run       model.Run           `json:"run"`
			Event     model.Event         `json:"event"`
			Session   *model.AgentSession `json:"session"`
			Turns     []model.AgentTurn   `json:"turns"`
			Artifacts []model.Artifact    `json:"artifacts"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &detail) != nil {
			t.Fatalf("run detail: %d %s", response.Code, response.Body)
		}
		return detail.Run, detail.Event, detail.Session, detail.Turns, detail.Artifacts
	}
	firstRun, firstEvent, firstSession, firstTurns, firstArtifacts := read()
	if completionErr != nil || !completed.Load() {
		t.Fatalf("concurrent completion failed: %v", completionErr)
	}
	if firstRun.Status != "running" || firstEvent.Status != "running" || firstSession == nil || firstSession.State != "running" || len(firstTurns) != 1 || firstTurns[0].Status != "running" || len(firstArtifacts) != 0 {
		t.Fatal("run detail mixed pre-completion and post-completion rows")
	}
	lastRun, lastEvent, lastSession, lastTurns, lastArtifacts := read()
	if lastRun.Status != "succeeded" || lastEvent.Status != "succeeded" || lastSession == nil || lastSession.State != "idle" || len(lastTurns) != 1 || lastTurns[0].Status != "completed" || len(lastArtifacts) != 1 {
		t.Fatal("completed run detail did not include its final artifacts and turn state")
	}
}
