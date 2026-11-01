package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server struct {
		Address       string `mapstructure:"address"`
		FrontendDir   string `mapstructure:"frontend_dir"`
		SecureCookies bool   `mapstructure:"secure_cookies"`
		PublicURL     string `mapstructure:"public_url"`
	} `mapstructure:"server"`
	Database struct {
		Driver string `mapstructure:"driver"`
		DSN    string `mapstructure:"dsn"`
	} `mapstructure:"database"`
	Admin struct {
		Username string `mapstructure:"username"`
		Password string `mapstructure:"password"`
	} `mapstructure:"admin"`
	Runtime struct {
		DataDir string `mapstructure:"data_dir"`
	} `mapstructure:"runtime"`
	Scheduler struct {
		PollInterval time.Duration `mapstructure:"poll_interval"`
	} `mapstructure:"scheduler"`
	Metrics struct {
		Address string `mapstructure:"address"`
	} `mapstructure:"metrics"`
}

// Load reads bootstrap configuration and CLOUDPILOT_ environment overrides.
func Load(path string) (Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	v.SetEnvPrefix("CLOUDPILOT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	defaults := map[string]any{
		"server.address": ":8080", "server.frontend_dir": "frontend/dist", "server.secure_cookies": false, "server.public_url": "",
		"database.driver": "sqlite", "database.dsn": "data/cloudpilot.db", "admin.username": "admin", "admin.password": "",
		"runtime.data_dir": "data/runs", "scheduler.poll_interval": "2s", "metrics.address": "",
	}
	for key, value := range defaults {
		v.SetDefault(key, value)
	}
	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := v.UnmarshalExact(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	if cfg.Server.Address == "" || cfg.Database.DSN == "" || cfg.Runtime.DataDir == "" {
		return cfg, errors.New("server.address, database.dsn and runtime.data_dir must not be empty")
	}
	if cfg.Scheduler.PollInterval < 100*time.Millisecond || cfg.Scheduler.PollInterval > time.Minute {
		return cfg, errors.New("scheduler.poll_interval must be between 100ms and 1m")
	}
	if cfg.Server.PublicURL != "" {
		u, err := url.Parse(cfg.Server.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(cfg.Server.PublicURL, "#") {
			return cfg, errors.New("server.public_url must be an absolute HTTP(S) URL without credentials, query or fragment")
		}
		cfg.Server.PublicURL = strings.TrimRight(cfg.Server.PublicURL, "/")
	}
	abs, err := filepath.Abs(cfg.Runtime.DataDir)
	if err != nil {
		return cfg, err
	}
	cfg.Runtime.DataDir = abs
	return cfg, nil
}
