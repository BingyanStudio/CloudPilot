package sandbox

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNovitaLive(t *testing.T) {
	keyFile := os.Getenv("CLOUDPILOT_SANDBOX_TEST_KEY_FILE")
	if keyFile == "" {
		t.Skip("set CLOUDPILOT_SANDBOX_TEST_KEY_FILE to run the live Novita test")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Provider: "novita", APIKey: strings.TrimSpace(string(key))}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	s, err := c.Create(ctx, CreateOptions{TemplateID: "base", Timeout: 5 * time.Minute, Metadata: map[string]string{"cloudpilot.test": "native-go"}, PauseOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile("/tmp/cloudpilot-novita-go-test-id", []byte(s.ID), 0o600); err != nil {
		t.Error(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.Kill(ctx, s.ID); err != nil {
			t.Error(err)
		} else {
			os.Remove("/tmp/cloudpilot-novita-go-test-id")
		}
	})
	t.Logf("created sandbox %s, envd %s", s.ID, s.EnvdVersion)
	if !s.SupportsStdinClose() {
		t.Fatal("template does not support stdin EOF")
	}
	data := []byte("binary\x00input\n")
	if err = s.Upload(ctx, "/tmp/cloudpilot-probe.bin", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	p, err := s.Start(ctx, Command{Path: "/bin/sh", Args: []string{"-c", "cat; printf diagnostic >&2; exit 7"}, Stdin: true, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	code, err := p.Wait(ctx)
	if err != nil || code != 7 || !bytes.Equal(stdout.Bytes(), data) || stderr.String() != "diagnostic" {
		t.Fatalf("process code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	if err = c.SetTimeout(ctx, s.ID, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = c.Pause(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	info, err := c.Get(ctx, s.ID)
	if err != nil || info.State != "paused" {
		t.Fatalf("pause not confirmed: %v %v", info, err)
	}
	items, err := c.List(ctx, map[string]string{"cloudpilot.test": "native-go"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.ID == s.ID && item.State == "paused" {
			found = true
		}
	}
	if !found {
		t.Fatal("paused sandbox missing from list")
	}
	s, err = c.Connect(ctx, s.ID, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Upload(ctx, "/tmp/cloudpilot-probe.bin", bytes.NewReader(data)); err != nil {
		t.Fatalf("overwrite resumed file: %v", err)
	}
	r, err := s.Download(ctx, "/tmp/cloudpilot-probe.bin")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(io.LimitReader(r, 1024))
	r.Close()
	if err != nil || !bytes.Equal(body, data) {
		t.Fatalf("restored file mismatch: %v", err)
	}
	p, err = s.Start(ctx, Command{Path: "/bin/sleep", Args: []string{"60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err = c.Kill(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Get(ctx, s.ID); !IsNotFound(err) {
		t.Fatalf("sandbox deletion not confirmed: %v", err)
	}
}
