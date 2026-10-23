package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/bootstrap"
	"github.com/ahsanulks/social-scheduler/internal/platform/config"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/fake"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/adapters/publishers/threads"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	slog.SetDefault(logger)

	publishers := []app.Publisher{}
	if cfg.FakeEnabled {
		f, err := fake.New(cfg.FakePlatform)
		if err != nil {
			logger.Error("failed to create fake publisher", "error", err)
			os.Exit(1)
		}
		publishers = append(publishers, f)
		logger.Info("fake publisher enabled", "platform", cfg.FakePlatform)
	}
	if cfg.Threads.Enabled {
		p, err := threads.New(threads.Config{
			UserID:      cfg.Threads.UserID,
			AccessToken: cfg.Threads.AccessToken,
			BaseURL:     cfg.Threads.BaseURL,
			DailyLimit:  cfg.Threads.DailyLimit,
		})
		if err != nil {
			logger.Error("failed to create threads publisher", "error", err)
			os.Exit(1)
		}
		publishers = append(publishers, p)
		logger.Info("threads publisher enabled", "user_id", cfg.Threads.UserID)
	}

	application, err := bootstrap.New(ctx, bootstrap.Config{
		DatabasePath:    cfg.DatabasePath,
		APIToken:        cfg.APIToken,
		DefaultPlatform: cfg.DefaultPlatform,
		Publishers:      publishers,
		Tick:            cfg.SchedulerTick,
		Batch:           cfg.SchedulerBatch,
		Lease:           cfg.Lease,
		MaxAttempts:     cfg.MaxAttempts,
		Logger:          logger,
	})
	if err != nil {
		logger.Error("failed to start application", "error", err)
		os.Exit(1)
	}
	defer application.Close()

	go application.Worker.Run(ctx)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.Handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
