// Package main is the entrypoint for the renovate-docker-operator binary.
// It wires together config, statestore, executor, discovery, cron, and graceful shutdown.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oluf-tech/renovate-docker-operator/config"
	"github.com/oluf-tech/renovate-docker-operator/internal/discovery"
	"github.com/oluf-tech/renovate-docker-operator/internal/executor"
	"github.com/oluf-tech/renovate-docker-operator/internal/scheduler"
	"github.com/oluf-tech/renovate-docker-operator/internal/server"
	"github.com/oluf-tech/renovate-docker-operator/internal/statestore"
)

func main() {
	// 1. Load configuration (fail fast on missing required vars)
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: failed to load config: %v\n", err)
		os.Exit(1)
	}

	// 2. Set up structured logger
	logLevel := parseLogLevel(cfg.LogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	logger.Info("renovate-docker-operator starting",
		"platform", cfg.Platform,
		"endpoint", cfg.PlatformEndpoint,
		"image", cfg.RenovateImage,
		"parallelism", cfg.Parallelism,
		"cron", cfg.CronSchedule,
	)

	// 3. Open SQLite state store (runs migrations and seeds default job if empty)
	store, err := statestore.New(cfg.SQLitePath, logger)
	if err != nil {
		logger.Error("failed to open state store", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	logger.Info("state store opened", "path", cfg.SQLitePath)

	// 4. Create Docker executor
	execCfg := executor.Config{
		Image:           cfg.RenovateImage,
		Network:         cfg.ContainerNetwork,
		CacheVolume:     cfg.CacheVolume,
		Parallelism:     cfg.Parallelism,
		JobTimeout:      cfg.JobTimeout,
		GracePeriod:     cfg.GracePeriod,
		ImagePullPolicy: cfg.ImagePullPolicy,
	}

	exec, err := executor.New(execCfg, store, logger)
	if err != nil {
		logger.Error("failed to create docker executor", "error", err)
		os.Exit(1)
	}

	// Verify Docker connectivity
	ctx := context.Background()
	if err := exec.Ping(ctx); err != nil {
		logger.Error("docker connectivity check failed", "error", err)
		os.Exit(1)
	}
	logger.Info("docker connectivity verified")

	// 5. Create Discovery agent
	disc := discovery.New(exec, store, logger)

	// 6. Start executor (dispatch loop + events listener)
	if err := exec.Start(ctx); err != nil {
		logger.Error("failed to start executor", "error", err)
		os.Exit(1)
	}

	// 7. Set up cron scheduler
	sched := scheduler.New(logger)
	if err := sched.AddFunc(cfg.CronSchedule, "discovery+dispatch", func() {
		runScheduledCycle(ctx, disc, store, logger, cfg.CronSkipDiscovery)
	}); err != nil {
		logger.Error("failed to add cron schedule", "error", err, "schedule", cfg.CronSchedule)
		os.Exit(1)
	}
	sched.Start()

	// 8. Start unified HTTP server (UI + webhook + health + API)
	srv := server.New(store, disc, sched, logger, "0.1.0")
	srv.Start()

	logger.Info("operator started — waiting for signals")

	// 9. Block on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh

	// 10. Graceful shutdown
	logger.Info("received signal, shutting down", "signal", sig)

	// Shutdown HTTP server first (stop accepting new requests)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	} else {
		logger.Info("HTTP server stopped")
	}

	sched.Stop()
	logger.Info("cron scheduler stopped")

	if err := exec.Stop(); err != nil {
		logger.Error("executor stop error", "error", err)
	} else {
		logger.Info("executor stopped")
	}

	if err := store.Close(); err != nil {
		logger.Error("state store close error", "error", err)
	} else {
		logger.Info("state store closed")
	}

	logger.Info("shutdown complete")
}

// runScheduledCycle runs discovery for all jobs, then the executor's dispatch
// loop will pick up newly scheduled projects on its next tick.
func runScheduledCycle(ctx context.Context, disc *discovery.DiscoveryAgent, store *statestore.SQLiteStore, logger *slog.Logger, skipDiscovery bool) {
	if skipDiscovery {
		logger.Info("cron fired but discovery is skipped (CRON_SKIP_DISCOVERY=true)")
		return
	}

	logger.Info("cron triggered: running discovery cycle")

	jobs, err := store.ListRenovateJobsFull(ctx)
	if err != nil {
		logger.Error("failed to list jobs for scheduled cycle", "error", err)
		return
	}

	for i := range jobs {
		job := &jobs[i]
		if _, err := disc.RunDiscovery(ctx, job); err != nil {
			logger.Error("discovery failed for job", "job", job.Name, "error", err)
		}
	}

	logger.Info("discovery cycle complete")
}

// parseLogLevel converts a string level to slog.Level.
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
