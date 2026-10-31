package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOverridesAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("database:\n  driver: sqlite\n  dsn: test.db\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDPILOT_ADMIN_PASSWORD", "environment-password")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Password != "environment-password" || cfg.Server.Address != ":8080" || !filepath.IsAbs(cfg.Runtime.DataDir) {
		t.Fatal("configuration defaults or overrides lost")
	}
	t.Setenv("CLOUDPILOT_SCHEDULER_POLL_INTERVAL", "0s")
	if _, err := Load(path); err == nil {
		t.Fatal("accepted unbounded poll interval")
	}
	t.Setenv("CLOUDPILOT_SCHEDULER_POLL_INTERVAL", "2s")
	if err := os.WriteFile(path, []byte("databse:\n  driver: sqlite\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted misspelled configuration")
	}
}
