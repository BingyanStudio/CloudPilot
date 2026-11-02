package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

func TestOutputCollection(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	files := map[string]string{
		"artifacts.json": `{"artifacts":[{"kind":"file","name":"APK","path":"build/app.apk"},{"kind":"pr","name":"Fix","url":"https://github.com/acme/app/pull/1"},{"kind":"text","name":"Note","content":"hidden-token"}]}`,
		"build/app.apk":  "apk bytes",
		"summary.md":     "Completed using hidden-token.",
	}
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := extractOutput(&archive, directory); err != nil {
		t.Fatal(err)
	}
	artifacts, err := readArtifacts(directory, 42, redactor([]string{"hidden-token"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 4 || artifacts[0].Size != 9 || artifacts[0].RunID != 42 || artifacts[2].Content != "[REDACTED]" || strings.Contains(artifacts[3].Content, "hidden-token") {
		t.Fatalf("unexpected artifacts: %+v", artifacts)
	}
}

func TestOutputRejectsEscapesAndLinks(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg, Size: 1},
		{Name: "/absolute", Typeflag: tar.TypeReg, Size: 1},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"},
		{Name: "large", Typeflag: tar.TypeReg, Size: maxFileSize + 1},
	} {
		t.Run(header.Name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if err := extractOutput(&archive, t.TempDir()); err == nil {
				t.Fatal("unsafe output accepted")
			}
		})
	}
}

func TestSecretEnvironmentAndLogRedaction(t *testing.T) {
	t.Setenv("TEST_CODEX_SECRET", "a-secret-key")
	t.Setenv("TEST_GIT_SECRET", "git-secret")
	task := engine.Task{Worker: model.WorkerSpec{APIKeySecretRef: "TEST_CODEX_SECRET"}, Workspace: model.WorkspaceSpec{SecretEnv: map[string]string{"GH_TOKEN": "TEST_GIT_SECRET"}}}
	prepared, err := (Codex{}).Prepare(task)
	if err != nil {
		t.Fatal(err)
	}
	environment, secrets, err := workerEnvironment(task, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(environment, "OPENAI_API_KEY=a-secret-key\n") || !strings.Contains(environment, "GH_TOKEN=git-secret\n") {
		t.Fatal("required environment missing")
	}
	var messages []string
	sink := &logSink{log: func(_, message string) { messages = append(messages, message) }, redact: redactor(secrets), buffers: map[string][]byte{}, dropping: map[string]bool{}}
	sink.write("stdout", []byte("split a-sec"))
	sink.write("stdout", []byte("ret-key and git-secret\n"))
	sink.flush()
	if len(messages) != 1 || messages[0] != "split [REDACTED] and [REDACTED]" {
		t.Fatalf("secret leaked: %q", messages)
	}
	// Resources stored before validation existed must still not read CloudPilot's own configuration.
	t.Setenv("CLOUDPILOT_ENCRYPTION_KEY", "master-key")
	reserved := task
	reserved.Worker.APIKeySecretRef = "CLOUDPILOT_ENCRYPTION_KEY"
	if _, err := (Codex{}).Prepare(reserved); err == nil {
		t.Fatal("worker credential read a reserved CloudPilot variable")
	}
	reserved = task
	reserved.Workspace.SecretEnv = map[string]string{"LEAK": "CLOUDPILOT_ENCRYPTION_KEY"}
	if _, _, err := workerEnvironment(reserved, prepared); err == nil {
		t.Fatal("workspace secretEnv read a reserved CloudPilot variable")
	}
	t.Setenv("TEST_CODEX_SECRET", "line1\nline2")
	prepared, err = (Codex{}).Prepare(task)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := workerEnvironment(task, prepared); err == nil {
		t.Fatal("multiline env value accepted")
	}
}

func TestDockerExecution(t *testing.T) {
	image := os.Getenv("CLOUDPILOT_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set CLOUDPILOT_TEST_DOCKER_IMAGE to the local fixture image")
	}
	t.Setenv("TEST_CODEX_SECRET", "fixture-secret-value")
	t.Setenv("TEST_GITHUB_SECRET", "fixture-github-token")
	executor := &Docker{DataDir: t.TempDir()}
	task := engine.Task{Run: model.Run{ID: 987654321}, Event: model.Event{Payload: json.RawMessage(`{"feedback":"fixture"}`)}, Worker: model.WorkerSpec{Type: "codex", APIKeySecretRef: "TEST_CODEX_SECRET", Model: "fixture"}, Runner: model.RunnerSpec{Type: "docker", Image: image, Network: "none"}}
	task.Workspace.SecretEnv = map[string]string{"GH_TOKEN": "TEST_GITHUB_SECRET"}
	var messages []string
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	instance, err := executor.instanceID()
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := exec.CommandContext(ctx, "docker", "run", "--detach", "--network", "none", "--label", "cloudpilot.instance="+instance, "--label", "cloudpilot.event=0", "--entrypoint", "node", image, "-e", "setInterval(()=>{},1000)").Output()
	if err != nil {
		t.Fatal(err)
	}
	orphanID := strings.TrimSpace(string(orphan))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", orphanID).Run() })
	artifacts, err := executor.Execute(ctx, task, func(_, message string) { messages = append(messages, message) })
	if err != nil {
		t.Fatalf("execution failed: %v\n%s", err, strings.Join(messages, "\n"))
	}
	if len(artifacts) != 3 {
		t.Fatalf("expected file, PR, summary: %+v", artifacts)
	}
	if err := exec.CommandContext(ctx, "docker", "inspect", orphanID).Run(); err == nil {
		t.Fatal("stale worker for the same event was not removed")
	}
	body, err := os.ReadFile(artifacts[0].Path)
	if err != nil || string(body) != "fixture APK" {
		t.Fatalf("bad artifact %q: %v", body, err)
	}
	if strings.Contains(strings.Join(messages, "\n"), "fixture-secret-value") {
		t.Fatal("credential leaked into logs")
	}
	if !strings.Contains(strings.Join(messages, "\n"), "[REDACTED]") {
		t.Fatal("fixture secret was not logged and redacted")
	}
	if !strings.HasPrefix(artifacts[0].Path, filepath.Join(executor.DataDir, "runs")) {
		t.Fatal("artifact escaped storage")
	}

	task.Run.ID++
	task.Workspace.Env = map[string]string{"FIXTURE_WAIT": "1"}
	cancelContext, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if _, err := executor.Execute(cancelContext, task, nil); err == nil {
		t.Fatal("cancellation should fail execution")
	}
	remaining, err := exec.Command("docker", "ps", "-aq", "--filter", "label=cloudpilot.run=987654322", "--filter", "label=cloudpilot.instance="+instance).Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(remaining)) != 0 {
		t.Fatalf("cancelled container was not removed: %s", remaining)
	}
}

func TestCodexProtocol(t *testing.T) {
	image := os.Getenv("CLOUDPILOT_TEST_CODEX_IMAGE")
	if image == "" {
		t.Skip("set CLOUDPILOT_TEST_CODEX_IMAGE to the built Codex image")
	}
	serverImage := os.Getenv("CLOUDPILOT_TEST_HTTP_IMAGE")
	if serverImage == "" {
		serverImage = image
	}
	t.Setenv("TEST_CODEX_SECRET", "fixture-protocol-key")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	network := "cloudpilot-protocol-" + strings.ToLower(filepath.Base(t.TempDir())) + "-" + time.Now().Format("150405000")
	command := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	command("network", "create", "--internal", network)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	server := command("create", "--network", network, "--network-alias", "responses", "--entrypoint", "node", serverImage, "/tmp/responses.mjs")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", server).Run() })
	command("cp", "testdata/responses.mjs", server+":/tmp/responses.mjs")
	command("start", server)
	command("exec", server, "node", "-e", "let ok=false;for(let i=0;i<20;i++){try{await fetch('http://127.0.0.1:8080/health');ok=true;break}catch{await new Promise(r=>setTimeout(r,100))}}if(!ok)process.exit(1)")
	task := engine.Task{
		Run: model.Run{ID: 987654323}, Event: model.Event{ID: 987654323, Payload: json.RawMessage(`{"feedback":"fixture"}`)},
		Worker:    model.WorkerSpec{Type: "codex", Model: "gpt-5.4", Endpoint: "http://responses:8080/v1", APIKeySecretRef: "TEST_CODEX_SECRET"},
		Workspace: model.WorkspaceSpec{Env: map[string]string{"NO_PROXY": "responses", "no_proxy": "responses"}},
		Runner:    model.RunnerSpec{Type: "docker", Image: image, Network: network},
	}
	var messages []string
	artifacts, err := (&Docker{DataDir: t.TempDir()}).Execute(ctx, task, func(_, message string) { messages = append(messages, message) })
	if err != nil {
		t.Fatalf("Codex protocol failed: %v\n%s", err, strings.Join(messages, "\n"))
	}
	if len(artifacts) != 1 || strings.TrimSpace(artifacts[0].Content) != "Verified local Codex protocol." {
		t.Fatalf("unexpected Codex output: %+v\n%s", artifacts, strings.Join(messages, "\n"))
	}
}
