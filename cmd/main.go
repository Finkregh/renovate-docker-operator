// Package main is the entrypoint for the renovate-docker-operator binary.
// It wires together config, statestore, executor, discovery, cron, and graceful shutdown.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/config"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/discovery"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/executor"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/imageupdate"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/metrics"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/resilience"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/scheduler"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/server"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/statestore"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/supervise"
)

// Version is set via -ldflags "-X main.Version=..." at build time.
var Version = "dev"

func main() {
	if err := run(os.Exit); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// run is main() minus os.Exit. exitFn is called synchronously on
// validation failures that must terminate the process without going
// through the graceful-shutdown path (e.g. bad ROP_IMAGE, bad
// ROP_IMAGE_UPDATE_POLL_INTERVAL). Tests inject a recording exitFn to
// assert the seam without actually killing the process.
func run(exitFn func(int)) error {
	// 1. Load configuration (fail fast on missing required vars)
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// 1b. Boot-time validation beyond individual env parses. Fatal via
	// the exitFn seam so tests can assert the wiring without actually
	// killing the process. Logging via slog isn't set up yet at this
	// point — print to stderr and let exitFn take over.
	if err := config.Validate(cfg, func(ref string) error {
		_, e := imageupdate.Parse(ref)
		return e
	}); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		exitFn(1)
		return err // in tests exitFn is a no-op; return so we don't proceed
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

	// 2b. Create metrics recorder
	metricsMode, err := metrics.ParseProjectLabelMode(cfg.MetricsProjectLabel)
	if err != nil {
		return fmt.Errorf("invalid ROP_METRICS_PROJECT_LABEL: %w", err)
	}
	metricsRec, err := metrics.New(metrics.Config{ProjectLabelMode: metricsMode})
	if err != nil {
		return fmt.Errorf("failed to create metrics recorder: %w", err)
	}
	logger.Info("metrics recorder created", "projectLabelMode", cfg.MetricsProjectLabel)

	// 2c. Create resilience manager
	resMgr := resilience.New(resilience.Config{
		FailureMinRuntime:  cfg.FailureMinRuntime,
		BackoffBase:        cfg.BackoffBase,
		BackoffMax:         cfg.BackoffMax,
		RapidFailWindow:    cfg.RapidFailWindow,
		RapidFailThreshold: cfg.RapidFailThreshold,
		ReplayQueueCap:     cfg.ReplayQueueCap,
	}, logger)
	logger.Info("resilience manager created",
		"rapidFailThreshold", cfg.RapidFailThreshold,
		"rapidFailWindow", cfg.RapidFailWindow,
		"backoffBase", cfg.BackoffBase,
		"backoffMax", cfg.BackoffMax,
	)

	// 3. Open SQLite state store (runs migrations and seeds default job if empty)
	store, err := statestore.New(cfg.SQLitePath, logger)
	if err != nil {
		return fmt.Errorf("failed to open state store: %w", err)
	}
	defer func() { _ = store.Close() }()
	logger.Info("state store opened", "path", cfg.SQLitePath)

	// 4. Create Docker executor
	execCfg := executor.Config{
		Image:                    cfg.RenovateImage,
		Network:                  cfg.ContainerNetwork,
		CacheVolume:              cfg.CacheVolume,
		ContainerbaseCacheVolume: cfg.ContainerbaseCacheVolume,
		Parallelism:              cfg.Parallelism,
		JobTimeout:               cfg.JobTimeout,
		GracePeriod:              cfg.GracePeriod,
		ImagePullPolicy:          cfg.ImagePullPolicy,
		ImageCacheTTL:            cfg.ImageCacheTTL,
		FailureMinRuntime:        cfg.FailureMinRuntime,
	}

	exec, err := executor.New(execCfg, store, logger)
	if err != nil {
		return fmt.Errorf("failed to create docker executor: %w", err)
	}
	exec.SetReporter(resMgr)
	exec.SetRecorder(metricsRec)

	// Verify Docker connectivity
	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	if err := exec.Ping(ctx); err != nil {
		return fmt.Errorf("docker connectivity check failed: %w", err)
	}
	logger.Info("docker connectivity verified")

	// 5. Create Discovery agent
	disc := discovery.New(exec, store, logger)

	// 6. Start executor (dispatch loop + events listener)
	if err := exec.Start(ctx); err != nil {
		return fmt.Errorf("failed to start executor: %w", err)
	}

	// 6b. Background image refresher.
	//
	// ROP_IMAGE_CACHE_TTL > 0 spawns a supervise.Loop that unconditionally
	// pulls the configured image on that cadence, keeping the local image
	// current so the dispatch hot-path never eats pull latency after idle.
	// The first tick runs immediately (supervise.Loop contract), providing
	// the startup probe. TTL == 0 disables the loop entirely; pullImage
	// falls back to its "image missing locally" safety net.
	if cfg.ImageCacheTTL > 0 {
		go supervise.Loop(ctx, logger, "image-refresher", cfg.ImageCacheTTL, exec.RefreshImage, supervise.WithMetrics(metricsRec), supervise.WithPerTickBody())
		logger.Info("image refresher started", "interval", cfg.ImageCacheTTL)
	} else {
		logger.Info("image refresher disabled (ROP_IMAGE_CACHE_TTL=0)")
	}

	// 6c. Image-update poller. Owned by the composition root; the
	// executor doesn't know about it and the poller doesn't know how
	// it's scheduled — supervise.Loop drives it.
	registryUA := "renovate-docker-operator/" + Version
	auth := imageupdate.NewAnonymousAuth(&http.Client{}, registryUA)
	registryClient := imageupdate.NewClient(registryUA, auth)
	poller := imageupdate.New(imageupdate.PollerConfig{
		Enabled:       cfg.CheckUpdateRunnerImage,
		ConfiguredRef: cfg.RenovateImage,
	}, registryClient, registryClient, exec, logger)
	if !cfg.CheckUpdateRunnerImage {
		poller.MarkDisabled()
		logger.Info("image update poller disabled (ROP_CHECK_UPDATE_RUNNER_IMAGE=false)")
	} else {
		// pollTick wraps poller.Tick so the image_update_available gauge is
		// refreshed from the poller's own snapshot after every tick. The
		// snapshot is always safe to read; on tick error UpdateAvailable keeps
		// its prior value, which is the correct behavior for the gauge.
		pollTick := func(c context.Context) error {
			err := poller.Tick(c)
			snap := poller.Snapshot()
			metricsRec.SetImageUpdateAvailable(snap.ConfiguredRef, snap.UpdateAvailable)
			return err
		}
		go supervise.Loop(ctx, logger, "image-update-poller", cfg.ImageUpdatePollInterval, pollTick, supervise.WithMetrics(metricsRec), supervise.WithPerTickBody())
		logger.Info("image update poller started",
			"interval", cfg.ImageUpdatePollInterval,
			"image", cfg.RenovateImage,
		)
	}

	// 7. Set up cron scheduler + dispatch gate
	sched := scheduler.New(logger)
	dispatcher := scheduler.NewDispatcher(resMgr, metricsRec, logger)
	if err := sched.AddFunc(cfg.CronSchedule, "discovery+dispatch", func() {
		runScheduledCycle(ctx, disc, store, exec, dispatcher, logger, cfg.CronSkipDiscovery)
	}); err != nil {
		return fmt.Errorf("failed to add cron schedule %q: %w", cfg.CronSchedule, err)
	}
	sched.Start()

	// 8. Start unified HTTP server (UI + webhook + health + API)
	srv := server.New(store, disc, sched, exec, logger, Version, cfg.MaxRequestBody)
	srv.SetResilience(resMgr)
	srv.SetMetrics(metricsRec)
	srv.SetPoller(poller)
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

	// Cancel the main context to signal all goroutines to stop
	ctxCancel()

	if err := exec.Stop(); err != nil {
		logger.Error("executor stop error", "error", err)
	} else {
		logger.Info("executor stopped")
	}

	logger.Info("shutdown complete")
	return nil
}

// runScheduledCycle runs discovery for all jobs, then dispatches projects
// through the resilience gate using the dispatcher.
func runScheduledCycle(ctx context.Context, disc *discovery.Agent, store *statestore.SQLiteStore, exec *executor.DockerExecutor, dispatcher *scheduler.Dispatcher, logger *slog.Logger, skipDiscovery bool) {
	if skipDiscovery {
		logger.Info("cron fired but discovery is skipped (ROP_CRON_SKIP_DISCOVERY=true)")
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
			continue
		}

		// Collect project names eligible for scheduling.
		var projectNames []string
		for _, p := range job.Status.Projects {
			if p.Status != api.JobStatusRunning {
				projectNames = append(projectNames, p.Name)
			}
		}

		logger.Info("dispatching projects after discovery", "job", job.Name, "candidates", len(projectNames))
		jobID := statestore.RenovateJobIdentifier{Name: job.Name}

		dispatched, dispErr := dispatcher.DispatchProjects(projectNames, resilience.SourceCron, func(project string) error {
			exec.SetProjectSource(project, resilience.SourceCron)
			return store.UpdateProjectStatus(ctx, project, jobID, &statestore.RenovateStatusUpdate{
				Status: api.JobStatusScheduled,
			})
		})
		if dispErr != nil {
			logger.Error("error during project dispatch", "job", job.Name, "error", dispErr)
		}
		logger.Info("dispatch complete", "job", job.Name, "dispatched", dispatched, "total", len(projectNames))
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
