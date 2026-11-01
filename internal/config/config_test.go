package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOverridesAndValidation(t *testing.T) {
	t.Setenv("CLOUDPILOT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("database:\n  driver: sqlite\n  dsn: test.db\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDPILOT_ADMIN_PASSWORD", "environment-password")
	t.Setenv("CLOUDPILOT_SERVER_PUBLIC_URL", "https://pilot.example.com/cloudpilot/")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Password != "environment-password" || cfg.Server.Address != ":8080" || !filepath.IsAbs(cfg.Runtime.DataDir) {
		t.Fatal("configuration defaults or overrides lost")
	}
	if cfg.Server.PublicURL != "https://pilot.example.com/cloudpilot" {
		t.Fatalf("invalid public URL override: %q", cfg.Server.PublicURL)
	}
	for _, value := range []string{"/relative", "ftp://pilot.example.com", "https://user:password@pilot.example.com", "https://pilot.example.com?q=1", "https://pilot.example.com/#/events", "https://pilot.example.com?", "https://pilot.example.com#"} {
		t.Setenv("CLOUDPILOT_SERVER_PUBLIC_URL", value)
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid public URL %q", value)
		}
	}
	t.Setenv("CLOUDPILOT_SERVER_PUBLIC_URL", "")
	t.Setenv("CLOUDPILOT_SCHEDULER_POLL_INTERVAL", "0s")
	if _, err := Load(path); err == nil {
		t.Fatal("accepted unbounded poll interval")
	}
	t.Setenv("CLOUDPILOT_SCHEDULER_POLL_INTERVAL", "2s")
	t.Setenv("CLOUDPILOT_ENCRYPTION_KEY", "")
	if _, err := Load(path); err == nil {
		t.Fatal("accepted missing encryption key")
	}
	t.Setenv("CLOUDPILOT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err := os.WriteFile(path, []byte("databse:\n  driver: sqlite\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted misspelled configuration")
	}
}
