package model_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestDatabaseIntegration requires an empty, disposable cloudpilot_test_* database.
func TestDatabaseIntegration(t *testing.T) {
	for _, backend := range []struct{ driver, env string }{
		{"postgres", "CLOUDPILOT_TEST_POSTGRES_DSN"},
		{"mysql", "CLOUDPILOT_TEST_MYSQL_DSN"},
	} {
		t.Run(backend.driver, func(t *testing.T) {
			dsn := os.Getenv(backend.env)
			if dsn == "" {
				t.Skip("set " + backend.env + " to an empty cloudpilot_test_* database")
			}
			db := integrationDatabase(t, backend.driver, dsn)
			large := strings.Repeat("feedback-修复\n", 10000)
			payload, err := json.Marshal(map[string]string{"feedback": large})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			resource := model.Resource{Kind: "Workspace", Name: "large-content", Spec: payload, Generation: 1}
			key := "delivery-1"
			event := model.Event{InputName: "feedback", PipelineName: "nightly", Status: "queued", Payload: payload, IdempotencyKey: &key, AvailableAt: now}
			for _, record := range []any{&resource, &event} {
				if err := db.Create(record).Error; err != nil {
					t.Fatal(err)
				}
			}
			run := model.Run{EventID: event.ID, PipelineName: "nightly", Status: "failed", Attempt: 1, Error: large, StartedAt: now}
			if err := db.Create(&run).Error; err != nil {
				t.Fatal(err)
			}
			log := model.RunLog{RunID: run.ID, Stream: "stdout", Message: large}
			artifact := model.Artifact{RunID: run.ID, Kind: "text", Name: "explanation", Content: large}
			for _, record := range []any{&log, &artifact} {
				if err := db.Create(record).Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, record := range []any{&resource, &event, &run, &log, &artifact} {
				if err := db.First(record).Error; err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(resource.Spec, payload) || !bytes.Equal(event.Payload, payload) || run.Error != large || log.Message != large || artifact.Content != large {
				t.Fatal("JSON or text larger than 64 KiB did not survive storage intact")
			}

			duplicateResource := model.Resource{Kind: resource.Kind, Name: resource.Name, Spec: payload}
			if err := db.Create(&duplicateResource).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
				t.Fatalf("resource uniqueness: %v", err)
			}
			duplicateEvent := model.Event{InputName: event.InputName, PipelineName: event.PipelineName, Status: "queued", Payload: payload, IdempotencyKey: &key, AvailableAt: now}
			if err := db.Create(&duplicateEvent).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
				t.Fatalf("delivery idempotency: %v", err)
			}
			for _, delivery := range []model.Event{
				{InputName: "another-input", IdempotencyKey: &key},
				{InputName: event.InputName},
				{InputName: event.InputName},
			} {
				delivery.PipelineName, delivery.Status, delivery.Payload, delivery.AvailableAt = "nightly", "queued", payload, now
				if err := db.Create(&delivery).Error; err != nil {
					t.Fatalf("input-scoped or nullable delivery key: %v", err)
				}
			}

			err = db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Create(&model.Resource{Kind: "Workspace", Name: "rolled-back", Spec: payload}).Error; err != nil {
					return err
				}
				duplicateEvent.ID = 0
				return tx.Create(&duplicateEvent).Error
			})
			if !errors.Is(err, gorm.ErrDuplicatedKey) {
				t.Fatalf("expected transactional uniqueness failure: %v", err)
			}
			var count int64
			if err := db.Model(&model.Resource{}).Where("name = ?", "rolled-back").Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("failed transaction did not roll back: count=%d, error=%v", count, err)
			}
			if err := db.Model(&model.Event{}).Count(&count).Error; err != nil || count != 4 {
				t.Fatalf("unexpected number of accepted deliveries: count=%d, error=%v", count, err)
			}
		})
	}
}

func integrationDatabase(t *testing.T, driver, dsn string) *gorm.DB {
	t.Helper()
	var dialect gorm.Dialector = postgres.Open(dsn)
	databaseQuery := "SELECT current_database()"
	if driver == "mysql" {
		dialect, databaseQuery = mysql.Open(dsn), "SELECT DATABASE()"
	}
	probe, err := gorm.Open(dialect, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	probeSQL, err := probe.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probeSQL.Close() })
	var database string
	if err := probe.Raw(databaseQuery).Scan(&database).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(database, "cloudpilot_test_") {
		t.Fatal("integration tests require a dedicated database named cloudpilot_test_*")
	}
	tables, err := probe.Migrator().GetTables()
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 0 {
		t.Fatal("integration database must be empty; refusing to modify existing tables")
	}
	t.Cleanup(func() {
		if err := probe.Migrator().DropTable(&model.PipelineState{}, &model.Artifact{}, &model.RunLog{}, &model.Run{}, &model.Event{}, &model.Session{}, &model.User{}, &model.Resource{}); err != nil {
			t.Error(err)
		}
	})
	db, err := model.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	// Reopening an already migrated database must preserve its schema.
	reopened, err := model.Open(driver, dsn)
	if err != nil {
		t.Fatalf("repeated migration: %v", err)
	}
	reopenedSQL, err := reopened.DB()
	if err != nil {
		t.Fatal(err)
	}
	_ = reopenedSQL.Close()
	return db
}
