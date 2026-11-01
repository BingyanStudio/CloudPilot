package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestRuntimeInstanceRecoveryPersistence(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "runtime.db")
	db, err := Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	runner := RunnerSpec{Type: "e2b", E2B: &E2BRunnerSpec{Provider: "novita", Domain: "us-phx-1.sandbox.novita.ai", APIKeySecretRef: "NOVITA_API_KEY", Template: "codex", LeaseSeconds: 900, IdleStrategy: "pause"}}
	snapshot, err := json.Marshal(runner)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(15 * time.Minute).Truncate(time.Second)
	instances := []RuntimeInstance{
		{DeploymentID: "deployment-a", EventID: 7, RunID: 1, AttemptID: "attempt-1", RunnerSnapshot: snapshot, State: "creating"},
		{DeploymentID: "deployment-a", EventID: 7, RunID: 2, SessionID: 3, AttemptID: "attempt-2", RunnerSnapshot: snapshot, SandboxID: "sandbox-2", ProcessID: 42, State: "running", ExpiresAt: &expires},
		{DeploymentID: "deployment-a", RunID: 4, AttemptID: "attempt-3", RunnerSnapshot: snapshot, State: "closed"},
		{DeploymentID: "deployment-b", RunID: 5, AttemptID: "attempt-4", RunnerSnapshot: snapshot, State: "running"},
	}
	if err := db.Create(&instances).Error; err != nil {
		t.Fatal(err)
	}
	duplicate := instances[0]
	duplicate.ID = 0
	if err := db.Create(&duplicate).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("duplicate execution attempt was accepted: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ = db.DB()
	var pending []RuntimeInstance
	if err := db.Where("deployment_id = ? AND state <> ?", "deployment-a", "closed").Order("id").Find(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].State != "creating" || pending[1].SandboxID != "sandbox-2" || pending[1].ProcessID != 42 || pending[1].SessionID != 3 || pending[0].EventID != 7 || pending[1].EventID != 7 {
		t.Fatalf("unexpected recovery records after restart: %+v", pending)
	}
	if pending[1].ExpiresAt == nil || !pending[1].ExpiresAt.Equal(expires) || !bytes.Equal(pending[1].RunnerSnapshot, snapshot) {
		t.Fatal("runtime lease or connection snapshot was not preserved")
	}
	var restored RunnerSpec
	if err := json.Unmarshal(pending[1].RunnerSnapshot, &restored); err != nil || restored.E2B == nil || restored.E2B.APIKeySecretRef != "NOVITA_API_KEY" {
		t.Fatalf("credential reference was not preserved: %v", err)
	}
	encoded, err := json.Marshal(pending[1])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("NOVITA_API_KEY")) || bytes.Contains(encoded, []byte("runnerSnapshot")) {
		t.Fatal("runtime JSON exposes private connection settings")
	}
}
