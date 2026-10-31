package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

const (
	maxFileSize   = 128 << 20
	maxOutputSize = 256 << 20
	maxTextSize   = 1 << 20
	maxArtifacts  = 100
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	if d.DataDir == "" {
		return "", errors.New("runtime data directory is required")
	}
	if err := os.MkdirAll(d.DataDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(d.DataDir, "runtime-id")
	value, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		var identifier [16]byte
		if _, err := rand.Read(identifier[:]); err != nil {
			return "", err
		}
		value = []byte(hex.EncodeToString(identifier[:]))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", err
		}
		_, writeErr := file.Write(value)
		closeErr := file.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	} else if err != nil {
		return "", err
	}
	if _, err := hex.DecodeString(string(value)); err != nil || len(value) != 32 {
		return "", errors.New("invalid runtime deployment ID")
	}
	d.instance = string(value)
	return d.instance, nil
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

func workerEnvironment(task engine.Task, prepared PreparedWorker) (string, []string, error) {
	env := map[string]string{}
	maps.Copy(env, task.Workspace.Env)
	var secrets []string
	for key, reference := range task.Workspace.SecretEnv {
		value, ok := os.LookupEnv(reference)
		if !ok || value == "" {
			return "", nil, fmt.Errorf("workspace secret %s is not configured", reference)
		}
		env[key] = value
		secrets = append(secrets, value)
	}
	maps.Copy(env, prepared.Environment)
	secrets = append(secrets, prepared.Secrets...)
	for _, server := range task.Workspace.MCPServers {
		for _, value := range server.Env {
			if value != "" {
				secrets = append(secrets, value)
			}
		}
	}
	keys := make([]string, 0, len(env))
	for key, value := range env {
		if !envName.MatchString(key) || strings.ContainsAny(value, "\r\n\x00") {
			return "", nil, fmt.Errorf("invalid environment entry %s", key)
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var result strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&result, "%s=%s\n", key, env[key])
	}
	return result.String(), secrets, nil
}

func redactor(secrets []string) func(string) string {
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	return func(value string) string {
		for _, secret := range secrets {
			if secret != "" {
				value = strings.ReplaceAll(value, secret, "[REDACTED]")
			}
		}
		return value
	}
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

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if left := 64<<10 - b.Len(); left > 0 {
		_, _ = b.Buffer.Write(p[:min(left, n)])
	}
	return n, nil
}

type logSink struct {
	mu           sync.Mutex
	log          func(string, string)
	redact       func(string) string
	buffers      map[string][]byte
	dropping     map[string]bool
	bytes, lines int
	limited      bool
}
type streamWriter struct {
	sink   *logSink
	stream string
}

func (w streamWriter) Write(p []byte) (int, error) { w.sink.write(w.stream, p); return len(p), nil }
func (s *logSink) write(stream string, p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		n := len(p)
		if i >= 0 {
			n = i
		}
		if !s.dropping[stream] {
			if len(s.buffers[stream])+n > maxTextSize {
				s.buffers[stream] = nil
				s.dropping[stream] = true
			} else {
				s.buffers[stream] = append(s.buffers[stream], p[:n]...)
			}
		}
		p = p[n:]
		if i < 0 {
			break
		}
		p = p[1:]
		if s.dropping[stream] {
			s.emit(stream, "[oversized log line omitted]")
		} else {
			s.emit(stream, string(s.buffers[stream]))
		}
		s.buffers[stream] = nil
		s.dropping[stream] = false
	}
}
func (s *logSink) emit(stream, message string) {
	if s.limited {
		return
	}
	message = s.redact(message)
	if len(message) > 16<<10 {
		message = message[:16<<10] + " [truncated]"
	}
	if s.lines >= 5000 || s.bytes+len(message) > 4<<20 {
		s.log("system", "Log limit reached; further worker output omitted.")
		s.limited = true
		return
	}
	s.lines++
	s.bytes += len(message)
	s.log(stream, message)
}
func (s *logSink) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for stream, message := range s.buffers {
		if len(message) > 0 {
			s.emit(stream, string(message))
		}
		delete(s.buffers, stream)
	}
}

func (d *Docker) collect(ctx context.Context, container string, runID uint, redact func(string) string) ([]model.Artifact, error) {
	base, err := filepath.Abs(d.DataDir)
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(base, "runs", strconv.FormatUint(uint64(runID), 10))
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, err
	}
	destination, err := os.MkdirTemp(runDir, "artifacts-")
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(destination)
		}
	}()
	command := exec.CommandContext(ctx, "docker", "cp", container+":/output/.", "-")
	pipe, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	extractErr := extractOutput(pipe, destination)
	_ = pipe.Close()
	if extractErr != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	commandErr := command.Wait()
	if extractErr != nil {
		return nil, fmt.Errorf("collect output: %w", extractErr)
	}
	if commandErr != nil {
		return nil, fmt.Errorf("copy output: %s", redact(stderr.String()))
	}
	artifacts, err := readArtifacts(destination, runID, redact)
	if err != nil {
		return nil, err
	}
	keep = true
	return artifacts, nil
}

func extractOutput(reader io.Reader, destination string) error {
	archive := tar.NewReader(io.LimitReader(reader, maxOutputSize+(4<<20)))
	var total int64
	files := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(header.Name)
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if !filepath.IsLocal(name) || strings.Contains(header.Name, "\\") {
			return errors.New("unsafe output path")
		}
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			files++
			total += header.Size
			if files > maxArtifacts+2 || header.Size > maxFileSize || total > maxOutputSize {
				return errors.New("worker output exceeds artifact limits")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, archive, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return errors.New("worker output may only contain regular files and directories")
		}
	}
}

type manifestEntry struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Path        string `json:"path"`
	Content     string `json:"content"`
	ContentType string `json:"contentType"`
}

func readArtifacts(directory string, runID uint, redact func(string) string) ([]model.Artifact, error) {
	var result []model.Artifact
	manifest, err := readSmallFile(filepath.Join(directory, "artifacts.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		var entries struct {
			Artifacts []manifestEntry `json:"artifacts"`
		}
		if err := json.Unmarshal(manifest, &entries); err != nil {
			return nil, fmt.Errorf("invalid artifact manifest: %w", err)
		}
		if len(entries.Artifacts) > maxArtifacts {
			return nil, errors.New("too many artifacts")
		}
		for _, entry := range entries.Artifacts {
			artifact := model.Artifact{RunID: runID, Kind: entry.Kind, Name: redact(entry.Name), URL: redact(entry.URL), Content: redact(entry.Content), ContentType: entry.ContentType}
			if artifact.Name == "" || len(artifact.Name) > 255 {
				return nil, errors.New("artifact name must contain 1 to 255 bytes")
			}
			switch entry.Kind {
			case "file":
				if !filepath.IsLocal(entry.Path) || entry.Path == "." || strings.Contains(entry.Path, "\\") {
					return nil, errors.New("unsafe artifact file path")
				}
				artifact.Path = filepath.Join(directory, entry.Path)
				info, err := os.Lstat(artifact.Path)
				if err != nil || !info.Mode().IsRegular() {
					return nil, errors.New("artifact file is missing or not regular")
				}
				artifact.Size = info.Size()
				if artifact.ContentType == "" {
					artifact.ContentType = "application/octet-stream"
				}
			case "pr", "issue":
				parsed, err := url.Parse(artifact.URL)
				if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
					return nil, errors.New("artifact URL must be HTTP or HTTPS without credentials")
				}
			case "text":
				if len(artifact.Content) > maxTextSize {
					return nil, errors.New("text artifact is too large")
				}
			default:
				return nil, fmt.Errorf("unsupported artifact kind %q", entry.Kind)
			}
			result = append(result, artifact)
		}
	}
	summary, err := readSmallFile(filepath.Join(directory, "summary.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(bytes.TrimSpace(summary)) > 0 {
		result = append(result, model.Artifact{RunID: runID, Kind: "text", Name: "Agent response", Content: redact(string(summary)), ContentType: "text/markdown"})
	}
	return result, nil
}
func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, maxTextSize+1))
	if err == nil && len(value) > maxTextSize {
		return nil, errors.New("artifact text exceeds 1 MiB")
	}
	return value, err
}
