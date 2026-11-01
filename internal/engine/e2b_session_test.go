package engine

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
)

func TestLostSandboxWorkspaceDisablesContinuationButAllowsClose(t *testing.T) {
	var closed atomic.Int32
	executor := &interactiveExecutor{started: make(chan *interactiveCall, 2), close: func() error { closed.Add(1); return nil }}
	db, scheduler := fixture(t, model.PipelineSpec{Concurrency: 1, TimeoutSeconds: 30}, executor)
	runner, err := model.FindResource(db, "Runner", "runner")
	if err != nil {
		t.Fatal(err)
	}
	runner.Spec = json.RawMessage(`{"type":"e2b","e2b":{"provider":"novita","template":"codex","apiKeySecretRef":"NOVITA_API_KEY"}}`)
	if err := db.Save(&runner).Error; err != nil {
		t.Fatal(err)
	}
	enqueue(t, db, time.Now().UTC())
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := nextTurn(t, executor)
	call.Finish <- false
	session := waitSession(t, db, call.Task.Run.ID, "idle")
	if err := db.Model(&session).Updates(map[string]any{"workspace_lost": true, "last_error": "sandbox expired"}).Error; err != nil {
		t.Fatal(err)
	}
	got, caps, err := scheduler.SessionInfo(call.Task.Run.ID)
	if err != nil || got == nil || !got.WorkspaceLost || !caps.Interactive || caps.CanResume || caps.CanSend || !caps.CanClose {
		t.Fatalf("lost workspace capabilities: session=%+v caps=%+v err=%v", got, caps, err)
	}
	if err := scheduler.Resume(call.Task.Run.ID, "continue", 1); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("resume accepted a lost workspace: %v", err)
	}
	if err := scheduler.SendMessage(call.Task.Run.ID, "additional instructions", 1); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("message queued a new turn in a lost workspace: %v", err)
	}
	var turns int64
	if err := db.Model(&model.AgentTurn{}).Where("session_id = ?", session.ID).Count(&turns).Error; err != nil || turns != 1 {
		t.Fatalf("rejected continuation created a turn: count=%d err=%v", turns, err)
	}
	if err := scheduler.CloseSession(call.Task.Run.ID); err != nil {
		t.Fatal(err)
	}
	waitSession(t, db, call.Task.Run.ID, "closed")
	if closed.Load() != 1 {
		t.Fatalf("lost workspace was not released exactly once: %d", closed.Load())
	}
}
