package model

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

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
