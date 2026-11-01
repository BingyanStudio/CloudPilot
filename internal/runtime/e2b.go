package runtime

import (
	"bytes"
	"context"
	"crypto/cipher"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/sandbox"
	"gorm.io/gorm"
)

// E2B runs workers in remote sandboxes and retains paused interactive workspaces.
type E2B struct {
	DB         *gorm.DB
	DataDir    string
	HTTPClient *http.Client
	Encryption cipher.AEAD
	mu         sync.Mutex
	active     map[uint]chan struct{}
}

//go:embed sandbox_process.mjs
var sandboxProcessScript []byte

// SupportsInteraction reports the worker protocol supported by this runner.
func (e *E2B) SupportsInteraction(task engine.Task) bool {
	return task.Runner.Type == "e2b" && task.Runner.E2B != nil && sessionWorker(task) != nil
}

func (e *E2B) claim(runID uint) (func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active[runID] != nil {
		return nil, errors.New("sandbox execution or cleanup is already active")
	}
	if e.active == nil {
		e.active = map[uint]chan struct{}{}
	}
	done := make(chan struct{})
	e.active[runID] = done
	return func() { e.mu.Lock(); delete(e.active, runID); close(done); e.mu.Unlock() }, nil
}

func (e *E2B) waitClaim(ctx context.Context, runID uint) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if release, err := e.claim(runID); err == nil {
			return release, nil
		}
		e.mu.Lock()
		done := e.active[runID]
		e.mu.Unlock()
		if done == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
		}
	}
}

func (e *E2B) client(runner model.RunnerSpec) (*sandbox.Client, error) {
	if e.DB == nil || runner.Type != "e2b" || runner.E2B == nil {
		return nil, errors.New("E2B runner requires a database and sandbox configuration")
	}
	spec := runner.E2B
	key, err := resolveAPIKey(e.Encryption, "Runner", spec.APIKey, spec.APIKeySecretRef)
	if err != nil {
		return nil, err
	}
	return &sandbox.Client{Provider: spec.Provider, Domain: spec.Domain, APIURL: spec.APIURL, APIKey: key, HTTPClient: e.HTTPClient}, nil
}

func sandboxLease(runner model.RunnerSpec) time.Duration {
	seconds := runner.E2B.LeaseSeconds
	if seconds <= 0 {
		seconds = 900
	}
	return time.Duration(seconds) * time.Second
}

func (e *E2B) execution(task engine.Task, prepared PreparedWorker, log func(string, string)) (*sandbox.Client, map[string]string, *logSink, []string, error) {
	client, err := e.client(task.Runner)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	env, secrets, err := resolveWorkerEnvironment(task, prepared)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	secrets = append(secrets, client.APIKey)
	if log == nil {
		log = func(string, string) {}
	}
	sink := &logSink{log: log, redact: redactor(secrets), buffers: map[string][]byte{}, dropping: map[string]bool{}}
	return client, env, sink, secrets, nil
}

// Execute runs a disposable sandbox, collecting output before deleting it.
func (e *E2B) Execute(ctx context.Context, task engine.Task, log func(string, string)) (artifacts []model.Artifact, err error) {
	prepared, err := prepareWorker(task)
	if err != nil {
		return nil, err
	}
	client, env, sink, _, err := e.execution(task, prepared, log)
	if err != nil {
		return nil, err
	}
	defer sink.flush()
	defer func() { err = redactError(err, sink.redact) }()
	release, err := e.waitClaim(ctx, task.Run.ID)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := e.clearPrevious(ctx, task); err != nil {
		return nil, err
	}
	record, sb, err := e.provision(ctx, client, task, 0)
	if record != nil {
		defer func() { err = errors.Join(err, e.cleanupRecord(record, false)) }()
	}
	if err != nil {
		return nil, err
	}
	runCtx, stopLease := e.renew(ctx, client, record, sandboxLease(task.Runner))
	defer stopLease()
	defer func() { err = sandboxRunError(ctx, runCtx, err) }()
	if err := e.prepare(runCtx, sb, prepared); err != nil {
		return nil, err
	}
	process, err := e.start(runCtx, sb, record, env, streamWriter{sink, "stdout"}, streamWriter{sink, "stderr"}, false)
	if err != nil {
		return nil, err
	}
	exitCode, err := process.Wait(runCtx)
	if err != nil {
		return nil, sandboxRunError(ctx, runCtx, err)
	}
	collectionCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	artifacts, err = e.collectSandbox(collectionCtx, sb, task.Run.ID, sink.redact)
	if err != nil {
		return artifacts, err
	}
	if exitCode != 0 {
		return artifacts, fmt.Errorf("worker exited with code %d", exitCode)
	}
	return artifacts, nil
}

// ExecuteTurn runs a fresh app-server process in the session's retained sandbox.
func (e *E2B) ExecuteTurn(ctx context.Context, task engine.Task, session model.AgentSession, turn model.AgentTurn, controls <-chan engine.SessionControl, emit func(engine.SessionUpdate), log func(string, string)) (result engine.TurnResult, err error) {
	if !e.SupportsInteraction(task) {
		return result, errors.New("worker does not support sandbox sessions")
	}
	if session.WorkspaceLost {
		return result, errors.New("session workspace is unavailable; close this session and start a new run")
	}
	prepared, err := sessionWorker(task).PrepareSession(task)
	if err != nil {
		return result, err
	}
	client, env, sink, secrets, err := e.execution(task, prepared, log)
	if err != nil {
		return result, err
	}
	defer sink.flush()
	defer func() { err = redactError(err, sink.redact) }()
	release, err := e.waitClaim(ctx, task.Run.ID)
	if err != nil {
		return result, err
	}
	defer release()
	record, sb, err := e.sessionSandbox(ctx, client, task, session)
	if record != nil && record.State != "lost" {
		defer func() { err = errors.Join(err, e.cleanupRecord(record, true)) }()
	}
	if err != nil {
		return result, err
	}
	if !sb.SupportsStdinClose() {
		return result, errors.New("interactive sessions require envd 0.5.2 or later; rebuild the sandbox template")
	}
	runCtx, stopLease := e.renew(ctx, client, record, sandboxLease(task.Runner))
	defer stopLease()
	defer func() { err = sandboxRunError(ctx, runCtx, err) }()
	if err := e.prepare(runCtx, sb, prepared); err != nil {
		return result, err
	}
	output, writer := io.Pipe()
	defer output.Close()
	process, err := e.start(runCtx, sb, record, env, writer, streamWriter{sink, "stderr"}, true)
	if err != nil {
		_ = writer.CloseWithError(err)
		return result, err
	}
	go func() {
		code, waitErr := process.Wait(runCtx)
		if waitErr == nil && code != 0 {
			waitErr = fmt.Errorf("app-server exited with code %d", code)
		}
		_ = writer.CloseWithError(waitErr)
	}()
	transport := sessionTransport{Input: process, Output: output, Collect: func(ctx context.Context) ([]model.Artifact, error) {
		return e.collectSandbox(ctx, sb, task.Run.ID, sink.redact)
	}}
	result, err = executeSession(runCtx, task, session, turn, controls, emit, transport, sink, secrets)
	err = sandboxRunError(ctx, runCtx, err)
	return result, err
}

func sandboxRunError(parent, runCtx context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if cause := context.Cause(runCtx); cause != nil {
		return cause
	}
	return err
}

func redactError(err error, redact func(string) string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New(redact(err.Error()))
}

func (e *E2B) prepare(ctx context.Context, sb *sandbox.Sandbox, prepared PreparedWorker) error {
	if prepared.Entrypoint == "" {
		return errors.New("worker entrypoint is required")
	}
	names := make([]string, 0, len(prepared.Files))
	for name := range prepared.Files {
		if !filepath.IsLocal(name) || filepath.Base(name) != name || name == "." || strings.ContainsAny(name, "\r\n\x00") || name == "cloudpilot-process.mjs" || name == "cloudpilot-process.json" {
			return errors.New("invalid worker bootstrap filename")
		}
		names = append(names, name)
	}
	slices.Sort(names)
	// Templates provide the runtime tools; this only resets per-turn output and bootstrap files.
	if err := sandboxCommand(ctx, sb, "sh", []string{"-c", "mkdir -p /workspace /output && find /output -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +"}, nil); err != nil {
		return fmt.Errorf("prepare sandbox directories (template must make /workspace and /output writable): %w", err)
	}
	// envd may open uploads as root before changing ownership; replacing a user-owned
	// file in sticky /tmp then fails with Linux protected_regular. Unlink as the worker first.
	remove := []string{"-f", "--", "/tmp/cloudpilot-process.mjs", "/tmp/cloudpilot-process.json"}
	for _, name := range names {
		remove = append(remove, "/tmp/"+name)
	}
	if err := sandboxCommand(ctx, sb, "rm", remove, nil); err != nil {
		return fmt.Errorf("replace worker bootstrap files: %w", err)
	}
	for _, name := range names {
		if err := sb.Upload(ctx, "/tmp/"+name, bytes.NewReader(prepared.Files[name])); err != nil {
			return fmt.Errorf("upload worker %s: %w", name, err)
		}
	}
	if err := sb.Upload(ctx, "/tmp/cloudpilot-process.mjs", bytes.NewReader(sandboxProcessScript)); err != nil {
		return err
	}
	configuration, err := json.Marshal(map[string]any{"entrypoint": prepared.Entrypoint, "args": prepared.Args})
	if err != nil {
		return err
	}
	return sb.Upload(ctx, "/tmp/cloudpilot-process.json", bytes.NewReader(configuration))
}

func (e *E2B) start(ctx context.Context, sb *sandbox.Sandbox, record *model.RuntimeInstance, env map[string]string, stdout, stderr io.Writer, stdin bool) (*sandbox.Process, error) {
	process, err := sb.Start(ctx, sandbox.Command{Path: "node", Args: []string{"/tmp/cloudpilot-process.mjs", "start"}, Dir: "/workspace", Env: env, Tag: "cloudpilot-" + record.AttemptID, Stdin: stdin, Stdout: stdout, Stderr: stderr})
	if err != nil {
		return nil, err
	}
	record.ProcessID = process.PID
	if err := e.DB.Model(record).Update("process_id", process.PID).Error; err != nil {
		return nil, fmt.Errorf("persist sandbox process: %w", err)
	}
	return process, nil
}

func sandboxCommand(ctx context.Context, sb *sandbox.Sandbox, path string, args []string, stdout io.Writer) error {
	var stderr limitedBuffer
	process, err := sb.Start(ctx, sandbox.Command{Path: path, Args: args, Stdout: stdout, Stderr: &stderr})
	if err != nil {
		return err
	}
	code, err := process.Wait(ctx)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return errors.Join(err, process.Stop(cleanup))
	}
	if code != 0 {
		return fmt.Errorf("sandbox command exited with code %d: %s", code, stderr.String())
	}
	return nil
}

func (e *E2B) collectSandbox(ctx context.Context, sb *sandbox.Sandbox, runID uint, redact func(string) string) ([]model.Artifact, error) {
	return collectOutput(e.DataDir, runID, redact, func(destination string) error {
		reader, writer := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := sandboxCommand(ctx, sb, "tar", []string{"-C", "/output", "-cf", "-", "."}, writer)
			_ = writer.CloseWithError(err)
			done <- err
		}()
		bounded := &io.LimitedReader{R: reader, N: maxOutputSize + (4 << 20) + 1}
		err := extractOutput(bounded, destination)
		if err == nil {
			_, err = io.Copy(io.Discard, bounded)
			if bounded.N == 0 {
				err = errors.New("worker output archive exceeds artifact limits")
			}
		}
		_ = reader.Close()
		return errors.Join(err, <-done)
	})
}

func (e *E2B) renew(parent context.Context, client *sandbox.Client, record *model.RuntimeInstance, lease time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(min(lease/3, time.Minute))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := client.SetTimeout(ctx, record.SandboxID, lease); err != nil {
					cancel(fmt.Errorf("sandbox lease renewal failed: %w", err))
					return
				}
				now := time.Now().UTC()
				if err := e.DB.Model(&model.RuntimeInstance{}).Where("id = ?", record.ID).Updates(map[string]any{"expires_at": now.Add(lease), "last_renewed_at": now}).Error; err != nil {
					cancel(fmt.Errorf("persist sandbox lease: %w", err))
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(nil); <-done }
}
