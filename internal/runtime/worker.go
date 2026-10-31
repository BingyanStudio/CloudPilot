package runtime

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

// PreparedWorker contains a container entrypoint and bootstrap files; resolved credentials belong only in Environment.
type PreparedWorker struct {
	Entrypoint  string
	Args        []string
	Files       map[string][]byte
	Environment map[string]string
	Secrets     []string
}

// Worker prepares an agent independently of the compute runner that launches it.
// SessionWorker prepares an agent with a bidirectional app-server transport.
type SessionWorker interface {
	PrepareSession(engine.Task) (PreparedWorker, error)
}

type Worker interface {
	Prepare(engine.Task) (PreparedWorker, error)
}

var workerAdapters = struct {
	sync.RWMutex
	items map[string]Worker
}{items: map[string]Worker{"codex": Codex{}}}

// RegisterWorker adds a worker adapter before the server starts accepting runs.
func RegisterWorker(name string, adapter Worker) error {
	if name == "" || adapter == nil {
		return errors.New("worker name and adapter are required")
	}
	workerAdapters.Lock()
	defer workerAdapters.Unlock()
	if _, exists := workerAdapters.items[name]; exists {
		return fmt.Errorf("worker %s already registered", name)
	}
	workerAdapters.items[name] = adapter
	return nil
}

func prepareWorker(task engine.Task) (PreparedWorker, error) {
	workerAdapters.RLock()
	adapter := workerAdapters.items[task.Worker.Type]
	workerAdapters.RUnlock()
	if adapter == nil {
		return PreparedWorker{}, fmt.Errorf("unsupported worker type %q", task.Worker.Type)
	}
	return adapter.Prepare(task)
}

//go:embed worker.mjs
var workerScript []byte

//go:embed session.mjs
var sessionScript []byte

//go:embed workspace.mjs
var workspaceScript []byte

// Codex prepares the Codex CLI adapter with an OpenAI-compatible Responses API provider.
type Codex struct{}

// Prepare resolves the worker credential and generates an isolated Codex bootstrap.
func (Codex) Prepare(task engine.Task) (PreparedWorker, error) {
	reference := task.Worker.APIKeySecretRef
	if reference == "" {
		return PreparedWorker{}, errors.New("worker apiKeySecretRef is required")
	}
	value, ok := os.LookupEnv(reference)
	if !ok || value == "" {
		return PreparedWorker{}, fmt.Errorf("worker secret %s is not configured", reference)
	}
	payload, err := json.Marshal(struct {
		Worker    model.WorkerSpec    `json:"worker"`
		Workspace model.WorkspaceSpec `json:"workspace"`
		Event     json.RawMessage     `json:"event"`
	}{task.Worker, task.Workspace, task.Event.Payload})
	if err != nil {
		return PreparedWorker{}, err
	}
	return PreparedWorker{
		Entrypoint: "node", Args: []string{"/tmp/cloudpilot-worker.mjs"},
		Files:       map[string][]byte{"cloudpilot-task.json": payload, "cloudpilot-worker.mjs": workerScript, "cloudpilot-workspace.mjs": workspaceScript},
		Environment: map[string]string{"OPENAI_API_KEY": value, "CODEX_HOME": "/workspace/.codex"},
		Secrets:     []string{value},
	}, nil
}

// PrepareSession adds the persistent-workspace app-server bootstrap.
func (c Codex) PrepareSession(task engine.Task) (PreparedWorker, error) {
	prepared, err := c.Prepare(task)
	if err != nil {
		return prepared, err
	}
	prepared.Args = []string{"/tmp/cloudpilot-session.mjs"}
	prepared.Files["cloudpilot-session.mjs"] = sessionScript
	return prepared, nil
}

func sessionWorker(task engine.Task) SessionWorker {
	workerAdapters.RLock()
	defer workerAdapters.RUnlock()
	adapter, _ := workerAdapters.items[task.Worker.Type].(SessionWorker)
	return adapter
}
