package runtime

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

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

// resolveAPIKey decrypts stored credentials and accepts legacy environment references.
func resolveAPIKey(encryption cipher.AEAD, purpose, encrypted, reference string) (string, error) {
	if encrypted != "" {
		return model.Decrypt(encryption, purpose, encrypted)
	}
	if value := os.Getenv(reference); value != "" {
		return value, nil
	}
	return "", fmt.Errorf("%s API key is not configured", purpose)
}

// resolveWorkerEnvironment resolves credentials without tying the values to a provider's transport.
func resolveWorkerEnvironment(task engine.Task, prepared PreparedWorker) (map[string]string, []string, error) {
	env := map[string]string{}
	maps.Copy(env, task.Workspace.Env)
	var secrets []string
	for key, reference := range task.Workspace.SecretEnv {
		value, ok := os.LookupEnv(reference)
		if !ok || value == "" {
			return nil, nil, fmt.Errorf("workspace secret %s is not configured", reference)
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
	for key, value := range env {
		if !envName.MatchString(key) || strings.ContainsAny(value, "\r\n\x00") {
			return nil, nil, fmt.Errorf("invalid environment entry %s", key)
		}
	}
	return env, secrets, nil
}

func workerEnvironment(task engine.Task, prepared PreparedWorker) (string, []string, error) {
	env, secrets, err := resolveWorkerEnvironment(task, prepared)
	if err != nil {
		return "", nil, err
	}
	var result strings.Builder
	for _, key := range slices.Sorted(maps.Keys(env)) {
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

var deploymentMu sync.Mutex

// deploymentID persists the identity used to reconcile this deployment's execution resources.
func deploymentID(dataDir string) (string, error) {
	deploymentMu.Lock()
	defer deploymentMu.Unlock()
	return readDeploymentID(dataDir)
}

func readDeploymentID(dataDir string) (string, error) {
	if dataDir == "" {
		return "", errors.New("runtime data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, "runtime-id")
	value, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		var identifier [16]byte
		if _, err := rand.Read(identifier[:]); err != nil {
			return "", err
		}
		value = []byte(hex.EncodeToString(identifier[:]))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return readDeploymentID(dataDir)
		}
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
	return string(value), nil
}
