package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

func TestStructuredEventsRedactSplitAndEscapedSecrets(t *testing.T) {
	secret := `token"with\escapes`
	var events []model.SessionEvent
	observer := &sessionObserver{emit: func(update engine.SessionUpdate) {
		if update.Event != nil {
			events = append(events, *update.Event)
		}
	}, redact: redactor([]string{secret}), secrets: []string{secret}, tails: map[string]string{}, completed: make(chan completedTurn, 1)}
	notify := func(method string, value any) { data, _ := json.Marshal(value); observer.notification(method, data) }
	notify("item/agentMessage/delta", map[string]any{"itemId": "message", "delta": "Hello " + secret[:6]})
	notify("item/agentMessage/delta", map[string]any{"itemId": "message", "delta": secret[6:] + " done"})
	notify("item/completed", map[string]any{"item": map[string]any{"id": "message", "type": "agentMessage", "text": "Hello " + secret + " done"}})
	notify("item/completed", map[string]any{"item": map[string]any{"id": "tool", "type": "mcpToolCall", "server": "fixture", "tool": "search", "status": "completed", "arguments": map[string]string{"token": secret}, "result": map[string]any{"content": []any{map[string]string{"text": secret}}}}})
	for _, event := range events {
		if strings.Contains(event.Text, secret) || strings.Contains(event.Command, "token\\\"") || strings.Contains(event.Output, "token\\\"") {
			t.Fatalf("secret escaped redaction: %+v", event)
		}
	}
	if len(events) != 4 || events[0].Text != "Hello " || events[1].Text != "[REDACTED] done" || !strings.Contains(events[3].Command, "[REDACTED]") || !strings.Contains(events[3].Output, "[REDACTED]") {
		t.Fatalf("unexpected normalized events: %+v", events)
	}
	if observer.summary() != "Hello [REDACTED] done" {
		t.Fatal("last completed message must be the summary")
	}
}

func TestInteractiveDockerSession(t *testing.T) {
	image := os.Getenv("CLOUDPILOT_TEST_SESSION_IMAGE")
	if image == "" {
		t.Skip("set CLOUDPILOT_TEST_SESSION_IMAGE to the interactive fixture image")
	}
	t.Setenv("TEST_SESSION_KEY", "fixture-session-secret")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	executor := &Docker{DataDir: dir}
	task := engine.Task{Run: model.Run{ID: 987654400}, Event: model.Event{ID: 987654400, Payload: json.RawMessage(`{"feedback":"wait for operator"}`)}, Worker: model.WorkerSpec{Type: "codex", Model: "fixture", APIKeySecretRef: "TEST_SESSION_KEY"}, Runner: model.RunnerSpec{Type: "docker", Image: image, Network: "none"}}
	session := model.AgentSession{RunID: task.Run.ID}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = executor.CloseSession(cleanupCtx, task, session)
	})
	controls := make(chan engine.SessionControl, 4)
	started := make(chan struct{}, 1)
	ack := make(chan engine.SessionUpdate, 1)
	var mu sync.Mutex
	var events []model.SessionEvent
	var logs []string
	emit := func(update engine.SessionUpdate) {
		mu.Lock()
		defer mu.Unlock()
		if update.Event != nil {
			events = append(events, *update.Event)
		}
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
	logger := func(_, message string) { mu.Lock(); defer mu.Unlock(); logs = append(logs, message) }
	type outcome struct {
		result engine.TurnResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := executor.ExecuteTurn(ctx, task, session, model.AgentTurn{ID: 1, Initial: true}, controls, emit, logger)
		done <- outcome{result, err}
	}()
	select {
	case <-started:
	case result := <-done:
		t.Fatalf("startup failed: %v; logs=%v", result.err, logs)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	controls <- engine.SessionControl{ID: 42, Type: "message", Text: "operator clarification"}
	select {
	case update := <-ack:
		if update.ControlError != "" {
			t.Fatal(update.ControlError)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	controls <- engine.SessionControl{Type: "interrupt"}
	var interrupted outcome
	select {
	case interrupted = <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if interrupted.err != nil || !interrupted.result.Interrupted || interrupted.result.ThreadID == "" {
		t.Fatalf("interrupt: %+v %v, logs=%v", interrupted.result, interrupted.err, logs)
	}
	session.ThreadID = interrupted.result.ThreadID
	executor = &Docker{DataDir: dir}
	if err := executor.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	continued, err := executor.ExecuteTurn(ctx, task, session, model.AgentTurn{ID: 2, Input: "continue"}, nil, emit, logger)
	if err != nil {
		t.Fatalf("resume: %v; logs=%v", err, logs)
	}
	if continued.ThreadID != session.ThreadID || len(continued.Artifacts) != 2 || !strings.Contains(continued.Artifacts[1].Content, "workspace counter 2") || !strings.Contains(continued.Artifacts[1].Content, "operator clarification") {
		t.Fatalf("state not preserved: %+v", continued)
	}
	priorPath := continued.Artifacts[0].Path
	again, err := executor.ExecuteTurn(ctx, task, session, model.AgentTurn{ID: 3, Input: "run again"}, nil, emit, logger)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(priorPath)
	if err != nil || string(content) != "turn 2" || again.Artifacts[0].Path == priorPath {
		t.Fatal("followup overwrote an earlier artifact")
	}
	mu.Lock()
	data, _ := json.Marshal(events)
	logText := strings.Join(logs, "\n")
	mu.Unlock()
	if strings.Contains(string(data), "fixture-session-secret") || strings.Contains(logText, "fixture-session-secret") {
		t.Fatal("secret leaked")
	}
	if !strings.Contains(string(data), "[REDACTED]") || !strings.Contains(string(data), `"tool":"shell"`) {
		t.Fatal("structured tool or redacted message missing")
	}
	if err := executor.CloseSession(ctx, task, session); err != nil {
		t.Fatal(err)
	}
	instance, _ := executor.instanceID()
	if err := exec.CommandContext(ctx, "docker", "volume", "inspect", sessionVolume(instance, task.Run.ID)).Run(); err == nil {
		t.Fatal("closed workspace volume remains")
	}
	if _, err := executor.ExecuteTurn(ctx, task, session, model.AgentTurn{ID: 4, Input: "continue"}, nil, emit, logger); err == nil {
		t.Fatal("missing workspace silently recreated for existing thread")
	}
}

func TestCodexInteractiveProtocol(t *testing.T) {
	image := os.Getenv("CLOUDPILOT_TEST_CODEX_IMAGE")
	if image == "" {
		t.Skip("set CLOUDPILOT_TEST_CODEX_IMAGE to the built Codex image")
	}
	serverImage := os.Getenv("CLOUDPILOT_TEST_HTTP_IMAGE")
	if serverImage == "" {
		serverImage = image
	}
	t.Setenv("TEST_SESSION_KEY", "fixture-protocol-key")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	network := "cloudpilot-appserver-" + time.Now().Format("150405000000")
	command("network", "create", "--internal", network)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	server := command("create", "--network", network, "--network-alias", "responses", "--entrypoint", "node", serverImage, "/tmp/responses.mjs")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", server).Run() })
	command("cp", "testdata/responses.mjs", server+":/tmp/responses.mjs")
	command("start", server)
	command("exec", server, "node", "-e", "let ok=false;for(let i=0;i<20;i++){try{await fetch('http://127.0.0.1:8080/health');ok=true;break}catch{await new Promise(r=>setTimeout(r,100))}}if(!ok)process.exit(1)")
	task := engine.Task{Run: model.Run{ID: 987654410}, Event: model.Event{ID: 987654410, Payload: json.RawMessage(`{"feedback":"fixture initial context"}`)}, Worker: model.WorkerSpec{Type: "codex", Model: "gpt-5.4", Endpoint: "http://responses:8080/v1", APIKeySecretRef: "TEST_SESSION_KEY"}, Workspace: model.WorkspaceSpec{Env: map[string]string{"NO_PROXY": "responses", "no_proxy": "responses"}}, Runner: model.RunnerSpec{Type: "docker", Image: image, Network: network}}
	dir := t.TempDir()
	executor := &Docker{DataDir: dir}
	session := model.AgentSession{RunID: task.Run.ID}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = executor.CloseSession(cleanupCtx, task, session)
	})
	var events []model.SessionEvent
	var logs []string
	emit := func(update engine.SessionUpdate) {
		if update.Event != nil {
			events = append(events, *update.Event)
		}
	}
	logger := func(_, message string) { logs = append(logs, message) }
	first, err := executor.ExecuteTurn(ctx, task, session, model.AgentTurn{ID: 1, Initial: true}, nil, emit, logger)
	if err != nil {
		t.Fatalf("initial Codex session: %v\n%s", err, strings.Join(logs, "\n"))
	}
	if first.ThreadID == "" || len(first.Artifacts) != 1 || first.Artifacts[0].Content != "Verified local Codex protocol." {
		t.Fatalf("unexpected initial response: %+v", first)
	}
	session.ThreadID = first.ThreadID
	executor = &Docker{DataDir: dir}
	second, err := executor.ExecuteTurn(ctx, task, session, model.AgentTurn{ID: 2, Input: "interactive follow-up"}, nil, emit, logger)
	if err != nil {
		t.Fatalf("resume Codex session: %v\n%s", err, strings.Join(logs, "\n"))
	}
	if second.ThreadID != first.ThreadID || len(second.Artifacts) != 1 || second.Artifacts[0].Content != "Verified resumed Codex conversation." {
		t.Fatalf("unexpected resumed response: %+v", second)
	}
	var assistantMessages int
	for _, event := range events {
		if event.Type == "message" && event.Role == "assistant" && event.Status == "completed" {
			assistantMessages++
		}
	}
	if assistantMessages != 2 {
		t.Fatalf("expected two normalized agent messages, got %+v", events)
	}
}
