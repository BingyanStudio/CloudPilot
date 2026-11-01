package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open connects to a supported database and applies the schema.
func Open(driver, dsn string) (*gorm.DB, error) {
	var dialect gorm.Dialector
	switch driver {
	case "sqlite":
		if dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") {
			if err := os.MkdirAll(filepath.Dir(strings.SplitN(dsn, "?", 2)[0]), 0700); err != nil {
				return nil, err
			}
		}
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dialect = sqlite.Open(dsn + sep + "_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on")
	case "postgres", "postgresql":
		dialect = postgres.Open(dsn)
	case "mysql":
		dialect = mysql.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported database driver %q", driver)
	}
	db, err := gorm.Open(dialect, &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent), NowFunc: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	if driver == "sqlite" {
		sqlDB.SetMaxOpenConns(1)
	} else {
		sqlDB.SetMaxOpenConns(20)
		sqlDB.SetMaxIdleConns(5)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
	}
	if err := db.AutoMigrate(&Resource{}, &User{}, &Session{}, &Event{}, &Run{}, &RunLog{}, &Artifact{}, &PipelineState{}, &AgentSession{}, &AgentTurn{}, &SessionEvent{}, &RuntimeInstance{}); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return db, nil
}

// BootstrapAdmin initializes the administrator once; subsequent starts keep stored credentials.
func BootstrapAdmin(db *gorm.DB, username, password string) error {
	var count int64
	if err := db.Model(&User{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if strings.TrimSpace(username) == "" || len(username) > 190 {
		return errors.New("admin.username must contain 1–190 characters")
	}
	if len(password) < 12 || len(password) > 72 {
		return errors.New("initial admin.password must contain 12–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return db.Create(&User{Username: username, PasswordHash: string(hash)}).Error
}
