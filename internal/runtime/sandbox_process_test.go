package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSandboxProcessLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the sandbox supervisor test")
	}
	script, err := filepath.Abs("sandbox_process.mjs")
	if err != nil {
		t.Fatal(err)
	}
	scriptURL := (&url.URL{Scheme: "file", Path: script}).String()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := func(t *testing.T, code, record string) *exec.Cmd {
		t.Helper()
		config, err := json.Marshal(map[string]any{"entrypoint": node, "args": []string{"-e", code}})
		if err != nil {
			t.Fatal(err)
		}
		return exec.CommandContext(ctx, node, "--input-type=module", "-e", `const {startWorker} = await import(process.argv[1]); process.exitCode = await startWorker(JSON.parse(process.argv[2]), process.argv[3]);`, scriptURL, string(config), record)
	}
	stop := func(record string) error {
		return exec.CommandContext(ctx, node, "--input-type=module", "-e", `const {stopWorker} = await import(process.argv[1]); await stopWorker(process.argv[2]);`, scriptURL, record).Run()
	}

	t.Run("EOF and separate streams", func(t *testing.T) {
		record := filepath.Join(t.TempDir(), "group")
		command := start(t, `process.stdin.pipe(process.stdout); process.stdin.on('end',()=>console.error('worker stderr'));`, record)
		command.Stdin = strings.NewReader("中文 JSONL\n")
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("supervisor: %v: %s", err, stderr.String())
		}
		if stdout.String() != "中文 JSONL\n" || stderr.String() != "worker stderr\n" {
			t.Fatalf("unexpected streams: stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("finished process record remains: %v", err)
		}
	})

	for _, detached := range []bool{false, true} {
		t.Run("background child cleanup detached="+strconv.FormatBool(detached), func(t *testing.T) {
			dir := t.TempDir()
			childPath, _ := json.Marshal(filepath.Join(dir, "child"))
			code := `const c=require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore',detached:` + strconv.FormatBool(detached) + `});require('node:fs').writeFileSync(` + string(childPath) + `,String(c.pid));c.unref();console.log('worker only');`
			command := start(t, code, filepath.Join(dir, "group"))
			output, err := command.CombinedOutput()
			if err != nil || string(output) != "worker only\n" {
				t.Fatalf("supervisor: %v: %q", err, output)
			}
			pid, err := os.ReadFile(filepath.Join(dir, "child"))
			if err != nil {
				t.Fatal(err)
			}
			stat, err := os.ReadFile("/proc/" + string(pid) + "/stat")
			if err == nil && !strings.HasPrefix(string(stat[strings.LastIndexByte(string(stat), ')')+2:]), "Z ") {
				t.Fatalf("background process %s is still running", pid)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}

	t.Run("orphan cleanup and identity check", func(t *testing.T) {
		record := filepath.Join(t.TempDir(), "group")
		command := start(t, `setInterval(()=>{},1000)`, record)
		var original []byte
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if json.Valid(original) {
				_ = os.WriteFile(record, original, 0o600)
			}
			_ = stop(record)
			_ = command.Process.Kill()
			_ = command.Wait()
		})
		for range 100 {
			original, err = os.ReadFile(record)
			if err == nil && json.Valid(original) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		var identity map[string]any
		if err := json.Unmarshal(original, &identity); err != nil {
			t.Fatal(err)
		}
		identity["startTime"] = "0"
		changed, _ := json.Marshal(identity)
		if err := os.WriteFile(record, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := stop(record); err == nil {
			t.Fatal("stop accepted a changed process identity")
		}
		pid := strconv.Itoa(int(identity["pid"].(float64)))
		if _, err := os.Stat("/proc/" + pid); err != nil {
			t.Fatalf("identity mismatch stopped the worker: %v", err)
		}
		if err := os.WriteFile(record, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := command.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err := command.Wait(); err == nil {
			t.Fatal("terminated supervisor returned success")
		}
		if _, err := os.Stat("/proc/" + pid); err != nil {
			t.Fatalf("fixture worker did not outlive the supervisor: %v", err)
		}
		if err := stop(record); err != nil {
			t.Fatal(err)
		}
		if err := stop(record); err != nil {
			t.Fatalf("stop is not idempotent: %v", err)
		}
	})
}
