package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/BingyanStudio/CloudPilot/internal/config"
	"github.com/BingyanStudio/CloudPilot/internal/controller"
	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/github"
	"github.com/BingyanStudio/CloudPilot/internal/metrics"
	appmiddleware "github.com/BingyanStudio/CloudPilot/internal/middleware"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/runtime"
	"github.com/BingyanStudio/CloudPilot/internal/views"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

func main() {
	path := flag.String("config", "config.yaml", "bootstrap YAML configuration path")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := serve(ctx, *path); err != nil {
		slog.Error("cloudpilot stopped", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	db, err := model.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	if err := model.BootstrapAdmin(db, cfg.Admin.Username, cfg.Admin.Password); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Runtime.DataDir, 0700); err != nil {
		return err
	}
	remote := &runtime.E2B{DB: db, DataDir: cfg.Runtime.DataDir}
	worker := engine.Create(db, runtime.Runners{"docker": &runtime.Docker{DataDir: cfg.Runtime.DataDir}, "e2b": remote})
	worker.PollInterval = cfg.Scheduler.PollInterval
	e := echo.New()
	e.HTTPErrorHandler = echo.DefaultHTTPErrorHandler(false)
	e.Use(middleware.Recover(), middleware.RequestID(), appmiddleware.Logger(), middleware.BodyLimit(2<<20))
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			h := c.Response().Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			return next(c)
		}
	})
	views.Register(e, &controller.App{DB: db, Engine: worker, DataDir: cfg.Runtime.DataDir, SecureCookies: cfg.Server.SecureCookies, PublicURL: cfg.Server.PublicURL}, cfg.Server.FrontendDir)
	listener, err := net.Listen("tcp", cfg.Server.Address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: e, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, WriteTimeout: 2 * time.Minute}
	var metricServer *http.Server
	var metricListener net.Listener
	if cfg.Metrics.Address != "" {
		registry := metrics.RegisterDatabase(sqlDB)
		e.Use(metrics.HTTPMiddleware(registry))
		metricListener, err = net.Listen("tcp", cfg.Metrics.Address)
		if err != nil {
			return fmt.Errorf("metrics listener: %w", err)
		}
		defer metricListener.Close()
		metricServer = &http.Server{Handler: metrics.Handler(registry), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}
	}
	if err := worker.Start(ctx); err != nil {
		return err
	}
	maintenanceCtx, stopMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); remote.Maintain(maintenanceCtx) }()
	defer func() { stopMaintenance(); <-maintenanceDone }()
	notificationCtx, stopNotifications := context.WithCancel(ctx)
	defer stopNotifications()
	notificationsDone := make(chan struct{})
	go func() {
		defer close(notificationsDone)
		(&github.Notifier{DB: db}).Run(notificationCtx)
	}()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.Serve(listener) }()
	if metricServer != nil {
		go func() { errorsCh <- metricServer.Serve(metricListener) }()
	}
	slog.Info("cloudpilot ready", "address", listener.Addr().String(), "database", cfg.Database.Driver)
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverErr := server.Shutdown(shutdownCtx)
	stopNotifications()
	select {
	case <-notificationsDone:
	case <-shutdownCtx.Done():
	}
	if metricServer != nil {
		_ = metricServer.Shutdown(shutdownCtx)
	}
	engineErr := worker.Stop(shutdownCtx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return errors.Join(serverErr, engineErr)
}
