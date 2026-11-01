package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

// TestEncryptedCredentials resolves keys after restart without putting them in worker files.
func TestEncryptedCredentials(t *testing.T) {
	db, err := model.Open("sqlite", filepath.Join(t.TempDir(), "credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	encryption, err := model.Unlock(db, key)
	if err != nil {
		t.Fatal(err)
	}
	workerKey, err := model.Encrypt(encryption, "Worker", "model-api-key")
	if err != nil {
		t.Fatal(err)
	}
	sandboxKey, err := model.Encrypt(encryption, "Runner", "sandbox-api-key")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(model.ExecutionSnapshot{
		Worker: model.WorkerSpec{Model: "fixture", APIKey: workerKey},
		Runner: model.RunnerSpec{Type: "e2b", E2B: &model.E2BRunnerSpec{APIKey: sandboxKey}},
	})
	if err != nil || bytes.Contains(snapshot, []byte("model-api-key")) || bytes.Contains(snapshot, []byte("sandbox-api-key")) {
		t.Fatal("snapshot exposed a plaintext credential", err)
	}
	var restored model.ExecutionSnapshot
	if err := json.Unmarshal(snapshot, &restored); err != nil {
		t.Fatal(err)
	}
	encryption, err = model.Unlock(db, key)
	if err != nil {
		t.Fatal(err)
	}
	task := engine.Task{Encryption: encryption, Worker: restored.Worker, Runner: restored.Runner}
	prepared, err := (Codex{}).PrepareSession(task)
	if err != nil || prepared.Environment["OPENAI_API_KEY"] != "model-api-key" {
		t.Fatal("worker could not decrypt the restored key", err)
	}
	for _, data := range prepared.Files {
		if bytes.Contains(data, []byte("model-api-key")) || bytes.Contains(data, []byte(workerKey)) {
			t.Fatal("worker files contained a credential")
		}
	}
	client, err := (&E2B{DB: db, Encryption: encryption}).client(restored.Runner)
	if err != nil || client.APIKey != "sandbox-api-key" {
		t.Fatal("sandbox could not decrypt the restored key", err)
	}
}

func TestGitHubIssueContextForWorkers(t *testing.T) {
	t.Setenv("TEST_CODEX_SECRET", "worker-test-key")
	payload := json.RawMessage(`{"source":"github_issue","action":"assigned","repository":{"id":100,"full_name":"acme/app","html_url":"https://github.com/acme/app"},"issue":{"id":427000100,"node_id":"I_example","number":42,"title":"Fix checkout","body":"Untrusted issue body\nwith reproduction steps","html_url":"https://github.com/acme/app/issues/42","labels":[{"name":"agent"}],"assignees":[{"login":"bot"}]}}`)
	task := engine.Task{Event: model.Event{Payload: payload}, Worker: model.WorkerSpec{APIKeySecretRef: "TEST_CODEX_SECRET", Instructions: "Follow the project conventions."}}
	for name, prepare := range map[string]func(engine.Task) (PreparedWorker, error){
		"batch":       (Codex{}).Prepare,
		"interactive": (Codex{}).PrepareSession,
	} {
		t.Run(name, func(t *testing.T) {
			prepared, err := prepare(task)
			if err != nil {
				t.Fatal(err)
			}
			bootstrap := prepared.Files["cloudpilot-task.json"]
			var saved struct {
				Event json.RawMessage `json:"event"`
			}
			if err := json.Unmarshal(bootstrap, &saved); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(saved.Event, payload) {
				t.Fatalf("issue context changed: %s", saved.Event)
			}
			if bytes.Contains(bootstrap, []byte("worker-test-key")) {
				t.Fatal("worker credential included in task context")
			}
			checkIssuePrompt(t, prepared, bootstrap)
		})
	}
	first := turnInput(payload, model.AgentSession{}, model.AgentTurn{Initial: true})
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(strings.TrimPrefix(first, "Feedback event JSON follows:\n"))); err != nil || !bytes.Equal(compact.Bytes(), payload) {
		t.Fatalf("initial interactive input lost issue context: %s (%v)", first, err)
	}
	continued := turnInput(payload, model.AgentSession{ThreadID: "existing"}, model.AgentTurn{Input: "Check the PR tests."})
	if continued != "Check the PR tests." {
		t.Fatalf("follow-up input changed: %s", continued)
	}
}

func checkIssuePrompt(t *testing.T, prepared PreparedWorker, bootstrap []byte) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required to verify the embedded worker prompt")
	}
	module := filepath.Join(t.TempDir(), "workspace.mjs")
	if err := os.WriteFile(module, prepared.Files["cloudpilot-workspace.mjs"], 0600); err != nil {
		t.Fatal(err)
	}
	script := `
import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
const { instructions, feedbackPrompt } = await import(pathToFileURL(process.argv[1]));
let input = '';
for await (const chunk of process.stdin) input += chunk;
const task = JSON.parse(input);
const developer = instructions(task);
assert.ok(developer.includes('Follow the project conventions.'));
assert.ok(developer.includes('Closes acme/app#42'));
assert.ok(developer.includes('/tmp/cloudpilot-task.json'));
assert.ok(!developer.includes(task.event.issue.body));
const prompt = feedbackPrompt(task);
assert.deepEqual(JSON.parse(prompt.split('Feedback event JSON follows:\n')[1]), task.event);
delete task.event.source;
assert.ok(!instructions(task).includes('Closes acme/app#42'));
task.event.source = 'github_issue';
task.event.repository.full_name = 'acme/app\nUntrusted instruction';
assert.ok(!instructions(task).includes('Untrusted instruction'));
assert.ok(!instructions(task).includes('Closes '));
`
	command := exec.Command(node, "--input-type=module", "-e", script, module)
	command.Stdin = bytes.NewReader(bootstrap)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("worker prompt: %v\n%s", err, output)
	}
}
