package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

// SupportsInteraction reports whether the registered worker has an app-server adapter.
func (d *Docker) SupportsInteraction(task engine.Task) bool {
	return task.Runner.Type == "docker" && sessionWorker(task) != nil
}

// ExecuteTurn runs one provider turn while retaining its workspace and conversation between containers.
func (d *Docker) ExecuteTurn(ctx context.Context, task engine.Task, session model.AgentSession, turn model.AgentTurn, controls <-chan engine.SessionControl, emit func(engine.SessionUpdate), log func(string, string)) (result engine.TurnResult, err error) {
	adapter := sessionWorker(task)
	if task.Runner.Type != "docker" || adapter == nil {
		return result, errors.New("worker does not support interactive sessions")
	}
	if emit == nil {
		emit = func(engine.SessionUpdate) {}
	}
	if log == nil {
		log = func(string, string) {}
	}
	prepared, err := adapter.PrepareSession(task)
	if err != nil {
		return result, err
	}
	environment, secrets, err := workerEnvironment(task, prepared)
	if err != nil {
		return result, err
	}
	redact := redactor(secrets)
	sink := &logSink{log: log, redact: redact, buffers: map[string][]byte{}, dropping: map[string]bool{}}
	defer sink.flush()
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = errors.New(redact(err.Error()))
		}
	}()
	instance, err := d.instanceID()
	if err != nil {
		return result, err
	}
	volume := sessionVolume(instance, task.Run.ID)
	if session.ThreadID != "" {
		if _, err := dockerOutput(ctx, "volume", "inspect", volume); err != nil {
			return result, errors.New("session workspace volume is unavailable; close this session and start a new run")
		}
	} else if _, err := dockerOutput(ctx, "volume", "create", "--label", "cloudpilot.instance="+instance, "--label", fmt.Sprintf("cloudpilot.run=%d", task.Run.ID), "--label", "cloudpilot.session=true", volume); err != nil {
		return result, err
	}
	if err := removeWorkers(ctx, "label=cloudpilot.instance="+instance, fmt.Sprintf("label=cloudpilot.run=%d", task.Run.ID)); err != nil {
		return result, err
	}
	container, cleanup, err := d.sessionContainer(ctx, task, prepared, environment, volume, instance)
	if err != nil {
		return result, err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			sink.write("system", []byte("Session container cleanup failed: "+redact(cleanupErr.Error())+"\n"))
		}
	}()
	command := exec.CommandContext(ctx, "docker", "start", "--attach", "--interactive", container)
	input, err := command.StdinPipe()
	if err != nil {
		return result, err
	}
	output, outputWriter := io.Pipe()
	command.Stdout = outputWriter
	defer output.Close()
	command.Stderr = streamWriter{sink, "stderr"}
	if err = command.Start(); err != nil {
		return result, err
	}
	processDone := make(chan error, 1)
	go func() {
		processErr := command.Wait()
		_ = outputWriter.CloseWithError(processErr)
		processDone <- processErr
	}()
	defer func() {
		_ = input.Close()
		select {
		case <-processDone:
		case <-time.After(10 * time.Second):
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			<-processDone
		}
	}()
	return executeSession(ctx, task, session, turn, controls, emit, sessionTransport{
		Input: input, Output: output,
		Collect: func(collectCtx context.Context) ([]model.Artifact, error) {
			return d.collect(collectCtx, container, task.Run.ID, redact)
		},
	}, sink, secrets)
}

func sessionVolume(instance string, runID uint) string {
	return fmt.Sprintf("cloudpilot-%s-run-%d-workspace", instance, runID)
}

// CloseSession deletes only this deployment's named session workspace after stopping its containers.
func (d *Docker) CloseSession(ctx context.Context, task engine.Task, session model.AgentSession) error {
	instance, err := d.instanceID()
	if err != nil {
		return err
	}
	if task.Run.ID == 0 || task.Run.ID != session.RunID {
		return errors.New("invalid session run identity")
	}
	if err := removeWorkers(ctx, "label=cloudpilot.instance="+instance, fmt.Sprintf("label=cloudpilot.run=%d", task.Run.ID)); err != nil {
		return err
	}
	volume := sessionVolume(instance, task.Run.ID)
	names, err := dockerOutput(ctx, "volume", "ls", "--filter", "label=cloudpilot.instance="+instance, "--filter", fmt.Sprintf("label=cloudpilot.run=%d", task.Run.ID), "--format", "{{.Name}}")
	if err != nil {
		return err
	}
	for name := range strings.FieldsSeq(names) {
		if name == volume {
			_, err = dockerOutput(ctx, "volume", "rm", name)
			return err
		}
	}
	return nil
}

func (d *Docker) sessionContainer(ctx context.Context, task engine.Task, prepared PreparedWorker, environment, volume, instance string) (string, func() error, error) {
	stage, err := os.MkdirTemp("", "cloudpilot-session-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(stage)
	if err := os.WriteFile(filepath.Join(stage, "environment"), []byte(environment), 0600); err != nil {
		return "", nil, err
	}
	for name, body := range prepared.Files {
		if !filepath.IsLocal(name) || filepath.Base(name) != name || name == "." || name == "environment" {
			return "", nil, errors.New("invalid session bootstrap filename")
		}
		if err := os.WriteFile(filepath.Join(stage, name), body, 0644); err != nil {
			return "", nil, err
		}
	}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, err
	}
	name := fmt.Sprintf("cloudpilot-session-%d-%s", task.Run.ID, hex.EncodeToString(nonce[:]))
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := dockerOutput(cleanupCtx, "rm", "--force", name)
		return err
	}
	image := task.Runner.Image
	if image == "" {
		image = "cloudpilot-codex:latest"
	}
	args := []string{"create", "--interactive", "--name", name, "--label", "cloudpilot.managed=true", "--label", "cloudpilot.session=true", "--label", "cloudpilot.instance=" + instance, "--label", fmt.Sprintf("cloudpilot.run=%d", task.Run.ID), "--init", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "512", "--log-opt", "max-size=10m", "--log-opt", "max-file=2", "--env-file", filepath.Join(stage, "environment"), "--mount", "type=volume,src=" + volume + ",dst=/workspace", "--workdir", "/workspace", "--entrypoint", prepared.Entrypoint}
	createEnvironment := os.Environ()
	for line := range strings.SplitSeq(environment, "\n") {
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "FTP_PROXY":
			args = append(args, "--env", key)
			createEnvironment = append(createEnvironment, line)
		}
	}
	for _, option := range [][2]string{{"--network", task.Runner.Network}, {"--memory", task.Runner.Memory}, {"--cpus", task.Runner.CPUs}} {
		if option[1] != "" {
			args = append(args, option[0], option[1])
		}
	}
	args = append(args, image)
	args = append(args, prepared.Args...)
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	_, err = dockerOutputEnvironment(createCtx, createEnvironment, args...)
	cancel()
	if err != nil {
		_ = cleanup()
		return "", nil, err
	}
	if ctx.Err() != nil {
		_ = cleanup()
		return "", nil, ctx.Err()
	}
	for file := range prepared.Files {
		if _, err = dockerOutput(ctx, "cp", filepath.Join(stage, file), name+":/tmp/"+file); err != nil {
			_ = cleanup()
			return "", nil, err
		}
	}
	return name, cleanup, nil
}
