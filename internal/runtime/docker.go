package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

// Docker executes workers in disposable containers without mounting the host workspace.
type Docker struct {
	DataDir  string
	mu       sync.Mutex
	instance string
}

// Recover removes this deployment's interrupted workers before their events can be retried.
func (d *Docker) Recover(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(d.DataDir, "runtime-id")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	instance, err := d.instanceID()
	if err != nil {
		return err
	}
	return removeWorkers(ctx, "label=cloudpilot.instance="+instance)
}

func removeWorkers(ctx context.Context, filters ...string) error {
	args := []string{"ps", "-aq"}
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}
	output, err := dockerOutput(ctx, args...)
	if err != nil {
		return fmt.Errorf("list interrupted workers: %w", err)
	}
	for id := range strings.FieldsSeq(output) {
		if _, err := dockerOutput(ctx, "rm", "--force", id); err != nil {
			return fmt.Errorf("remove interrupted worker: %w", err)
		}
	}
	return nil
}

func (d *Docker) instanceID() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.instance != "" {
		return d.instance, nil
	}
	value, err := deploymentID(d.DataDir)
	if err != nil {
		return "", err
	}
	d.instance = value
	return value, nil
}

// Execute provisions a worker, streams logs, and copies bounded output from the stopped container.
func (d *Docker) Execute(ctx context.Context, task engine.Task, log func(string, string)) ([]model.Artifact, error) {
	if task.Runner.Type != "docker" {
		return nil, errors.New("unsupported runner type")
	}
	prepared, err := prepareWorker(task)
	if err != nil {
		return nil, err
	}
	if prepared.Entrypoint == "" {
		return nil, errors.New("worker entrypoint is required")
	}
	environment, secrets, err := workerEnvironment(task, prepared)
	if err != nil {
		return nil, err
	}
	redact := redactor(secrets)
	if log == nil {
		log = func(string, string) {}
	}
	sink := &logSink{log: log, redact: redact, buffers: map[string][]byte{}, dropping: map[string]bool{}}
	defer sink.flush()
	stage, err := os.MkdirTemp("", "cloudpilot-run-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	if err := os.WriteFile(filepath.Join(stage, "environment"), []byte(environment), 0o600); err != nil {
		return nil, err
	}
	for name, body := range prepared.Files {
		if !filepath.IsLocal(name) || filepath.Base(name) != name || name == "." || name == "environment" {
			return nil, errors.New("invalid worker bootstrap filename")
		}
		if err := os.WriteFile(filepath.Join(stage, name), body, 0o644); err != nil {
			return nil, err
		}
	}
	instance, err := d.instanceID()
	if err != nil {
		return nil, err
	}
	if err := removeWorkers(ctx, "label=cloudpilot.instance="+instance, fmt.Sprintf("label=cloudpilot.event=%d", task.Event.ID)); err != nil {
		return nil, err
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("cloudpilot-%d-%s", task.Run.ID, hex.EncodeToString(random[:]))
	image := task.Runner.Image
	if image == "" {
		image = "cloudpilot-codex:latest"
	}
	args := []string{"create", "--name", name, "--label", "cloudpilot.managed=true", "--label", "cloudpilot.instance=" + instance, "--label", fmt.Sprintf("cloudpilot.event=%d", task.Event.ID), "--label", fmt.Sprintf("cloudpilot.run=%d", task.Run.ID), "--init", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "512", "--log-opt", "max-size=10m", "--log-opt", "max-file=2", "--env-file", filepath.Join(stage, "environment"), "--workdir", "/workspace", "--entrypoint", prepared.Entrypoint}
	// Docker client proxy defaults otherwise take precedence over values from an env file.
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
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := dockerOutput(cleanup, "rm", "--force", name); err != nil {
			sink.write("system", []byte("Container cleanup failed: "+redact(err.Error())+"\n"))
		}
	}()
	// Wait for Docker to acknowledge creation before cleanup: cancelling the client can otherwise leave a container created later by the daemon.
	createContext, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	_, createErr := dockerOutputEnvironment(createContext, createEnvironment, args...)
	cancelCreate()
	if createErr != nil {
		return nil, fmt.Errorf("create worker: %s", redact(createErr.Error()))
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	for file := range prepared.Files {
		if _, err := dockerOutput(ctx, "cp", filepath.Join(stage, file), name+":/tmp/"+file); err != nil {
			return nil, fmt.Errorf("prepare worker: %s", redact(err.Error()))
		}
	}
	command := exec.CommandContext(ctx, "docker", "start", "--attach", name)
	command.Stdout = streamWriter{sink, "stdout"}
	command.Stderr = streamWriter{sink, "stderr"}
	startErr := command.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Docker start --attach normally returns the container exit code; inspect also covers transport interruptions.
	status, inspectErr := dockerOutput(ctx, "inspect", "--format", "{{.State.Running}} {{.State.ExitCode}}", name)
	if inspectErr != nil {
		return nil, fmt.Errorf("inspect worker: %s", redact(inspectErr.Error()))
	}
	fields := strings.Fields(status)
	if len(fields) != 2 || fields[0] != "false" {
		return nil, errors.New("worker attachment ended before the container stopped")
	}
	artifacts, artifactErr := d.collect(ctx, name, task.Run.ID, redact)
	if artifactErr != nil {
		return artifacts, fmt.Errorf("collect artifacts: %s", redact(artifactErr.Error()))
	}
	if fields[1] != "0" || startErr != nil {
		return artifacts, fmt.Errorf("worker exited with code %s", fields[1])
	}
	return artifacts, nil
}

func dockerOutput(ctx context.Context, args ...string) (string, error) {
	return dockerOutputEnvironment(ctx, nil, args...)
}

func dockerOutputEnvironment(ctx context.Context, environment []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	command.Env = environment
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(output.String()))
	}
	return strings.TrimSpace(output.String()), nil
}

func (d *Docker) collect(ctx context.Context, container string, runID uint, redact func(string) string) ([]model.Artifact, error) {
	return collectOutput(d.DataDir, runID, redact, func(destination string) error {
		command := exec.CommandContext(ctx, "docker", "cp", container+":/output/.", "-")
		pipe, err := command.StdoutPipe()
		if err != nil {
			return err
		}
		var stderr limitedBuffer
		command.Stderr = &stderr
		if err := command.Start(); err != nil {
			return err
		}
		extractErr := extractOutput(pipe, destination)
		_ = pipe.Close()
		if extractErr != nil && command.Process != nil {
			_ = command.Process.Kill()
		}
		commandErr := command.Wait()
		if extractErr != nil {
			return fmt.Errorf("collect output: %w", extractErr)
		}
		if commandErr != nil {
			return fmt.Errorf("copy output: %s", redact(stderr.String()))
		}
		return nil
	})
}
