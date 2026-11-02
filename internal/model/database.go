package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
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
		dialect = sqlite.Open(dsn + sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
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
	if err := db.AutoMigrate(&Resource{}, &User{}, &Session{}, &Event{}, &Run{}, &RunLog{}, &Artifact{}, &PipelineState{}, &AgentSession{}, &AgentTurn{}, &SessionEvent{}, &RuntimeInstance{}, &GitHubIssueNotification{}, &OIDCSettings{}, &encryptionState{}); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return db, nil
}

// BootstrapAdmin initializes the administrator once; subsequent starts keep stored credentials.
// Databases created before roles existed promote their oldest user so an administrator always remains.
func BootstrapAdmin(db *gorm.DB, username, password string) error {
	var count int64
	if err := db.Model(&User{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		var admins int64
		if err := db.Model(&User{}).Where("role = ?", RoleAdmin).Count(&admins).Error; err != nil || admins > 0 {
			return err
		}
		var oldest User
		if err := db.Order("id ASC").First(&oldest).Error; err != nil {
			return err
		}
		return db.Model(&oldest).Update("role", RoleAdmin).Error
	}
	if strings.TrimSpace(username) == "" || len(username) > 190 {
		return errors.New("admin.username must contain 1–190 characters")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("initial admin.password: %w", err)
	}
	return db.Create(&User{Username: username, PasswordHash: hash, Role: RoleAdmin}).Error
}

// HashPassword enforces the local-account password policy before hashing.
func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 72 {
		return "", errors.New("password must contain 12–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}
