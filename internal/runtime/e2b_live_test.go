package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/sandbox"
)

type e2bLiveRun struct {
	executor *E2B
	task     engine.Task
	session  model.AgentSession
	client   *sandbox.Client
	journal  string
	mu       sync.Mutex
	logs     []string
	events   []model.SessionEvent
}

func openE2BLiveRun(t *testing.T) *e2bLiveRun {
	t.Helper()
	keyFile, template := os.Getenv("CLOUDPILOT_E2B_TEST_KEY_FILE"), os.Getenv("CLOUDPILOT_E2B_TEST_TEMPLATE")
	if keyFile == "" || template == "" {
		t.Skip("set CLOUDPILOT_E2B_TEST_KEY_FILE and CLOUDPILOT_E2B_TEST_TEMPLATE for live Novita runtime tests")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIVE_SANDBOX_KEY", strings.TrimSpace(string(key)))
	t.Setenv("LIVE_MODEL_KEY", strings.TrimSpace(string(key)))
	t.Setenv("LIVE_FIXTURE_KEY", "fixture-session-secret")
	dir := t.TempDir()
	db, err := model.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	live := &e2bLiveRun{executor: &E2B{DB: db, DataDir: dir}, session: model.AgentSession{RunID: 1, State: "idle"}}
	live.task = engine.Task{Run: model.Run{ID: 1}, Event: model.Event{ID: 1, Payload: json.RawMessage(`{"feedback":"wait for operator"}`)}, Worker: model.WorkerSpec{Type: "codex", Model: "fixture", APIKeySecretRef: "LIVE_FIXTURE_KEY"}, Runner: model.RunnerSpec{Type: "e2b", E2B: &model.E2BRunnerSpec{Provider: "novita", Domain: "us-phx-1.sandbox.novita.ai", APIKeySecretRef: "LIVE_SANDBOX_KEY", Template: template, LeaseSeconds: 300, IdleStrategy: "pause"}}}
	if err = db.Create(&live.session).Error; err != nil {
		t.Fatal(err)
	}
	live.client, err = live.executor.client(live.task.Runner)
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := live.executor.deployment()
	if err != nil {
		t.Fatal(err)
	}
	live.journal = filepath.Join(os.TempDir(), "cloudpilot-e2b-live-"+deployment+".json")
	live.record(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		live.record(t)
		cleanupTask := live.task
		cleanupTask.Run.ID = live.session.RunID
		if err := live.executor.CloseSession(ctx, cleanupTask, live.session); err != nil {
			t.Errorf("close session cleanup: %v", err)
		}
		remaining, err := live.client.List(ctx, map[string]string{"cloudpilot_instance": deployment})
		if err != nil {
			t.Errorf("verify cloud cleanup: %v", err)
			return
		}
		for _, sb := range remaining {
			t.Errorf("runtime left sandbox %s in state %s; removing test resource", sb.ID, sb.State)
			if err := live.client.Kill(ctx, sb.ID); err != nil {
				t.Errorf("remove test sandbox: %v", err)
				return
			}
		}
		remaining, err = live.client.List(ctx, map[string]string{"cloudpilot_instance": deployment})
		if err != nil || len(remaining) != 0 {
			t.Errorf("test cloud resources remain: count=%d error=%v", len(remaining), err)
			return
		}
		_ = os.Remove(live.journal)
	})
	return live
}

func (l *e2bLiveRun) record(t *testing.T) {
	t.Helper()
	var records []model.RuntimeInstance
	if err := l.executor.DB.Find(&records).Error; err != nil {
		t.Error(err)
		return
	}
	body, err := json.Marshal(struct {
		DataDir   string
		Instances []model.RuntimeInstance
	}{l.executor.DataDir, records})
	if err != nil {
		t.Error(err)
		return
	}
	if err = os.WriteFile(l.journal, body, 0o600); err != nil {
		t.Error(err)
	}
}

func (l *e2bLiveRun) emit(update engine.SessionUpdate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if update.Event != nil {
		l.events = append(l.events, *update.Event)
	}
	if update.ThreadID != "" {
		_ = l.executor.DB.Model(&model.AgentSession{}).Where("id = ?", l.session.ID).Update("thread_id", update.ThreadID).Error
	}
}

func (l *e2bLiveRun) log(stream, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logs = append(l.logs, stream+": "+message)
}

func (l *e2bLiveRun) diagnostics() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.logs, "\n")
}

func (l *e2bLiveRun) instance(t *testing.T) model.RuntimeInstance {
	t.Helper()
	var record model.RuntimeInstance
	if err := l.executor.DB.Where("run_id = ?", l.task.Run.ID).Order("id DESC").First(&record).Error; err != nil {
		t.Fatal(err)
	}
	l.record(t)
	return record
}

func (l *e2bLiveRun) paused(t *testing.T, ctx context.Context) string {
	t.Helper()
	record := l.instance(t)
	if record.State != "paused" || record.ProcessID != 0 {
		t.Fatalf("runtime did not settle: state=%s pid=%d error=%s", record.State, record.ProcessID, record.LastError)
	}
	info, err := l.client.Get(ctx, record.SandboxID)
	if err != nil || info.State != "paused" {
		t.Fatalf("provider pause not confirmed: %v %v", info, err)
	}
	return record.SandboxID
}

type e2bSessionFixture struct{ script []byte }

func (f e2bSessionFixture) Prepare(task engine.Task) (PreparedWorker, error) {
	return (Codex{}).Prepare(task)
}

func (f e2bSessionFixture) PrepareSession(task engine.Task) (PreparedWorker, error) {
	prepared, err := (Codex{}).PrepareSession(task)
	if err != nil {
		return prepared, err
	}
	prepared.Files["fixture-codex.mjs"] = f.script
	prepared.Files["cloudpilot-session.mjs"] = bytes.Replace(sessionScript, []byte("spawn('codex', ['app-server', '--listen', 'stdio://']"), []byte("spawn('node', ['/tmp/fixture-codex.mjs', 'app-server']"), 1)
	return prepared, nil
}

func registerE2BLiveWorker(t *testing.T, name string, worker Worker) {
	t.Helper()
	if err := RegisterWorker(name, worker); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { workerAdapters.Lock(); delete(workerAdapters.items, name); workerAdapters.Unlock() })
}

func TestE2BLiveInteractiveFixture(t *testing.T) {
	live := openE2BLiveRun(t)
	script, err := os.ReadFile("testdata/session/codex.mjs")
	if err != nil {
		t.Fatal(err)
	}
	registerE2BLiveWorker(t, "e2b-live-session", e2bSessionFixture{script: script})
	live.task.Worker.Type = "e2b-live-session"
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	controls := make(chan engine.SessionControl, 4)
	started := make(chan struct{}, 1)
	ack := make(chan engine.SessionUpdate, 4)
	emit := func(update engine.SessionUpdate) {
		live.emit(update)
		if update.ProviderTurnID != "" {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		if update.ControlID != 0 {
			ack <- update
		}
	}
	type outcome struct {
		result engine.TurnResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := live.executor.ExecuteTurn(ctx, live.task, live.session, model.AgentTurn{ID: 1, Initial: true}, controls, emit, live.log)
		done <- outcome{result, err}
	}()
	select {
	case <-started:
	case result := <-done:
		t.Fatalf("start: %v\n%s", result.err, live.diagnostics())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	live.record(t)
	controls <- engine.SessionControl{ID: 42, Type: "message", Text: "operator clarification"}
	select {
	case update := <-ack:
		if update.ControlID != 42 || update.ControlError != "" {
			t.Fatalf("steer failed: %+v", update)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	controls <- engine.SessionControl{Type: "interrupt"}
	var first outcome
	select {
	case first = <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if first.err != nil || !first.result.Interrupted || first.result.ThreadID == "" {
		t.Fatalf("interrupt: %v %+v\n%s", first.err, first.result, live.diagnostics())
	}
	live.session.ThreadID = first.result.ThreadID
	sandboxID := live.paused(t, ctx)
	live.executor = &E2B{DB: live.executor.DB, DataDir: live.executor.DataDir}
	if err := live.executor.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := live.executor.ExecuteTurn(ctx, live.task, live.session, model.AgentTurn{ID: 2, Input: "continue"}, nil, live.emit, live.log)
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, live.diagnostics())
	}
	if second.ThreadID != live.session.ThreadID || len(second.Artifacts) != 2 || !strings.Contains(second.Artifacts[1].Content, "workspace counter 2") || !strings.Contains(second.Artifacts[1].Content, "operator clarification") {
		t.Fatalf("state not retained: %+v", second)
	}
	if live.paused(t, ctx) != sandboxID {
		t.Fatal("resume silently replaced sandbox")
	}
	priorPath := second.Artifacts[0].Path
	third, err := live.executor.ExecuteTurn(ctx, live.task, live.session, model.AgentTurn{ID: 3, Input: "run again"}, nil, live.emit, live.log)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := os.ReadFile(priorPath)
	if err != nil || string(prior) != "turn 2" || len(third.Artifacts) < 1 || third.Artifacts[0].Path == priorPath {
		t.Fatal("later turn overwrote prior artifacts")
	}
	live.mu.Lock()
	events, _ := json.Marshal(live.events)
	live.mu.Unlock()
	if strings.Contains(string(events), "fixture-session-secret") || strings.Contains(live.diagnostics(), "fixture-session-secret") {
		t.Fatal("worker secret leaked")
	}
	if !strings.Contains(string(events), "[REDACTED]") || !strings.Contains(string(events), `"tool":"shell"`) {
		t.Fatal("redacted tool timeline missing")
	}
	if err = live.executor.CloseSession(ctx, live.task, live.session); err != nil {
		t.Fatal(err)
	}
	if _, err = live.client.Get(ctx, sandboxID); !sandbox.IsNotFound(err) {
		t.Fatalf("closed sandbox remains: %v", err)
	}
}

type e2bBatchFixture struct{ script string }

func (f e2bBatchFixture) Prepare(engine.Task) (PreparedWorker, error) {
	return PreparedWorker{Entrypoint: "node", Args: []string{"/tmp/batch-fixture.mjs"}, Files: map[string][]byte{"batch-fixture.mjs": []byte(f.script)}}, nil
}

func TestE2BLiveBatchAndCancellation(t *testing.T) {
	live := openE2BLiveRun(t)
	registerE2BLiveWorker(t, "e2b-live-batch", e2bBatchFixture{script: `import fs from 'node:fs'; fs.writeFileSync('/output/probe.txt','batch output'); fs.writeFileSync('/output/artifacts.json',JSON.stringify({artifacts:[{kind:'file',name:'batch',path:'probe.txt'}]})); process.exitCode=7;`})
	live.task.Worker.Type = "e2b-live-batch"
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	artifacts, err := live.executor.Execute(ctx, live.task, live.log)
	if err == nil || !strings.Contains(err.Error(), "exited with code 7") || len(artifacts) != 1 {
		t.Fatalf("batch exit/artifacts: %v %+v\n%s", err, artifacts, live.diagnostics())
	}
	content, err := os.ReadFile(artifacts[0].Path)
	if err != nil || string(content) != "batch output" {
		t.Fatal("batch output lost")
	}
	first := live.instance(t)
	if first.State != "closed" {
		t.Fatalf("batch cleanup pending: %s", first.State)
	}
	if _, err = live.client.Get(ctx, first.SandboxID); !sandbox.IsNotFound(err) {
		t.Fatalf("batch sandbox still exists: %v", err)
	}
	registerE2BLiveWorker(t, "e2b-live-cancel", e2bBatchFixture{script: `console.log('cancellation-ready'); setInterval(()=>{},1000);`})
	live.task.Worker.Type = "e2b-live-cancel"
	live.task.Run.ID = 2
	live.task.Event.ID = 2
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := live.executor.Execute(runCtx, live.task, func(stream, message string) {
			live.log(stream, message)
			if strings.Contains(message, "cancellation-ready") {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		})
		done <- err
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("cancel startup: %v\n%s", err, live.diagnostics())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	live.record(t)
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation result: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := live.instance(t)
	if second.State != "closed" {
		t.Fatalf("cancel cleanup pending: %s", second.State)
	}
	if _, err = live.client.Get(ctx, second.SandboxID); !sandbox.IsNotFound(err) {
		t.Fatalf("cancelled sandbox still exists: %v", err)
	}
	live.task.Run.ID = live.session.RunID
}

func TestE2BLiveCodexDeepSeek(t *testing.T) {
	live := openE2BLiveRun(t)
	live.task.Worker = model.WorkerSpec{Type: "codex", Model: "deepseek/deepseek-v4.1-flash-1017", Endpoint: "https://api.novita.ai/openai/v1", APIKeySecretRef: "LIVE_MODEL_KEY", Instructions: "This is a small runtime integration test. Use shell commands only for the requested files. Do not inspect repositories or make network calls. Keep reasoning and replies brief."}
	live.task.Event.Payload = json.RawMessage(`{"feedback":"Use a shell command to write exactly e2b-runtime-state (no newline) to /workspace/live-state.txt and /output/round-one.txt. Write /output/artifacts.json containing {\"artifacts\":[{\"kind\":\"file\",\"name\":\"round one\",\"path\":\"round-one.txt\"}]}. Reply briefly when done."}`)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	first, err := live.executor.ExecuteTurn(ctx, live.task, live.session, model.AgentTurn{ID: 1, Initial: true}, nil, live.emit, live.log)
	if err != nil {
		t.Fatalf("first real model turn: %v\n%s", err, live.diagnostics())
	}
	if first.ThreadID == "" {
		t.Fatal("model thread ID missing")
	}
	var firstPath string
	for _, artifact := range first.Artifacts {
		if artifact.Kind == "file" {
			firstPath = artifact.Path
			break
		}
	}
	content, err := os.ReadFile(firstPath)
	if err != nil || string(content) != "e2b-runtime-state" {
		t.Fatalf("first model artifact failed: %v", err)
	}
	live.session.ThreadID = first.ThreadID
	sandboxID := live.paused(t, ctx)
	live.executor = &E2B{DB: live.executor.DB, DataDir: live.executor.DataDir}
	if err = live.executor.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := live.executor.ExecuteTurn(ctx, live.task, live.session, model.AgentTurn{ID: 2, Input: "Read /workspace/live-state.txt. Copy its exact contents to /output/round-two.txt. Write /output/artifacts.json containing {\"artifacts\":[{\"kind\":\"file\",\"name\":\"round two\",\"path\":\"round-two.txt\"}]}. Do not rewrite the workspace file. Reply briefly."}, nil, live.emit, live.log)
	if err != nil {
		t.Fatalf("resumed real model turn: %v\n%s", err, live.diagnostics())
	}
	if second.ThreadID != first.ThreadID {
		t.Fatal("resume changed Codex thread ID")
	}
	var secondPath string
	for _, artifact := range second.Artifacts {
		if artifact.Kind == "file" {
			secondPath = artifact.Path
			break
		}
	}
	content, err = os.ReadFile(secondPath)
	if err != nil || string(content) != "e2b-runtime-state" || secondPath == firstPath {
		t.Fatalf("resumed model artifact failed: %v", err)
	}
	if content, err = os.ReadFile(firstPath); err != nil || string(content) != "e2b-runtime-state" {
		t.Fatal("earlier model artifact overwritten")
	}
	if live.paused(t, ctx) != sandboxID {
		t.Fatal("model resume replaced sandbox")
	}
	if err = live.executor.CloseSession(ctx, live.task, live.session); err != nil {
		t.Fatal(err)
	}
	if _, err = live.client.Get(ctx, sandboxID); !sandbox.IsNotFound(err) {
		t.Fatalf("model sandbox remains after close: %v", err)
	}
	t.Logf("verified real Codex/DeepSeek two-turn conversation, thread %s", first.ThreadID)
}
