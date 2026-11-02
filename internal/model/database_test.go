package model

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// TestSQLiteReopen verifies connection settings and persistence through repeated migration.
func TestSQLiteReopen(t *testing.T) {
	for _, backend := range []struct{ name, prefix, query string }{
		{"path", "", ""},
		{"uri", "file:", "?cache=private"},
	} {
		t.Run(backend.name, func(t *testing.T) {
			dsn := backend.prefix + filepath.Join(t.TempDir(), "cloudpilot.db") + backend.query
			now := time.Date(2026, time.November, 1, 12, 15, 22, 123456789, time.UTC)
			resource := Resource{
				Kind: "Worker", Name: "codex", Spec: json.RawMessage(`{"type":"codex"}`),
				Generation: 1, CreatedAt: now, UpdatedAt: now,
			}
			for attempt := range 2 {
				db, err := Open("sqlite", dsn)
				if err != nil {
					t.Fatal(err)
				}
				sqlDB, err := db.DB()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = sqlDB.Close() })
				// Discard idle connections so each query checks a newly opened connection.
				sqlDB.SetMaxIdleConns(0)
				for _, pragma := range []struct{ query, want string }{
					{"PRAGMA journal_mode", "wal"},
					{"PRAGMA busy_timeout", "5000"},
					{"PRAGMA foreign_keys", "1"},
				} {
					var got string
					if err := sqlDB.QueryRow(pragma.query).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != pragma.want {
						t.Fatalf("%s = %q, want %q", pragma.query, got, pragma.want)
					}
				}
				if attempt == 0 {
					if err := db.Create(&resource).Error; err != nil {
						t.Fatal(err)
					}
				} else {
					got, err := FindResource(db, resource.Kind, resource.Name)
					if err != nil {
						t.Fatal(err)
					}
					if got.ID != resource.ID || got.Generation != resource.Generation || !bytes.Equal(got.Spec, resource.Spec) || !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
						t.Fatalf("resource changed after reopening: %+v", got)
					}
				}
				if err := sqlDB.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestBootstrapAndJSONPersistence(t *testing.T) {
	db, err := Open("sqlite", filepath.Join(t.TempDir(), "cloudpilot.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := BootstrapAdmin(db, "admin", "short"); err == nil {
		t.Fatal("accepted weak bootstrap password")
	}
	if err := BootstrapAdmin(db, "admin", "initial-password"); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(db, "other", "changed-password"); err != nil {
		t.Fatal(err)
	}
	var users []User
	if err := db.Find(&users).Error; err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || bcrypt.CompareHashAndPassword([]byte(users[0].PasswordHash), []byte("initial-password")) != nil {
		t.Fatal("restart changed bootstrapped administrator")
	}
	r := Resource{Kind: "Worker", Name: "codex", Spec: json.RawMessage(`{"type":"codex","model":"gpt-5"}`), Generation: 1}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	got, err := FindResource(db, "Worker", "codex")
	if err != nil {
		t.Fatal(err)
	}
	var spec WorkerSpec
	if err := got.Decode(&spec); err != nil || spec.Type != "codex" {
		t.Fatalf("JSON persistence: %v %+v", err, spec)
	}
	copy := Resource{Kind: "Worker", Name: "codex", Spec: r.Spec}
	if err := db.Create(&copy).Error; err != gorm.ErrDuplicatedKey {
		t.Fatalf("unique resource key: %v", err)
	}
}

// TestArtifactOutputPath verifies stored artifact paths map back to the worker's /output layout.
func TestArtifactOutputPath(t *testing.T) {
	for path, want := range map[string]string{
		"/data/runs/runs/7/artifacts-123/app.apk":      "app.apk",
		"/data/runs/runs/7/artifacts-123/logs/a.log":   "logs/a.log",
		"/data/artifacts-x/runs/7/artifacts-1/out.txt": "out.txt",
		"": "",
	} {
		artifact := Artifact{Path: path}
		_ = artifact.AfterFind(nil)
		if artifact.OutputPath != want {
			t.Fatalf("%q -> %q, want %q", path, artifact.OutputPath, want)
		}
	}
}
