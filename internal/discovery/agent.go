// Package discovery handles project autodiscovery from git platforms.
// Runs the Renovate discovery container and parses its output.
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/executor"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/statestore"
)

// Agent manages project discovery for RenovateJobs.
type Agent struct {
	executor *executor.DockerExecutor
	store    statestore.RenovateJobManager
	logger   *slog.Logger
}

// New creates a new discovery Agent.
func New(exec *executor.DockerExecutor, store statestore.RenovateJobManager, logger *slog.Logger) *Agent {
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{
		executor: exec,
		store:    store,
		logger:   logger,
	}
}

// RunDiscovery runs autodiscovery for the given job and reconciles projects.
// Returns the list of removed projects (for webhook cleanup).
func (d *Agent) RunDiscovery(ctx context.Context, job *api.RenovateJob) (removed []string, err error) {
	d.logger.Info("starting discovery", "job", job.Name)

	// 1. Run discovery container to get repo list
	repos, err := d.executor.DispatchDiscovery(ctx, job)
	if err != nil {
		return nil, fmt.Errorf("dispatch discovery: %w", err)
	}

	d.logger.Info("discovery found repos", "job", job.Name, "count", len(repos))

	// 2. Reconcile projects in state store
	removed, err = d.store.ReconcileProjects(ctx, job, repos)
	if err != nil {
		return nil, fmt.Errorf("reconcile projects: %w", err)
	}

	if len(removed) > 0 {
		d.logger.Info("projects removed during reconciliation",
			"job", job.Name, "removed", removed)
	}

	// 3. Return removed for webhook sync
	return removed, nil
}

// RunDiscoveryAsync starts an asynchronous discovery run.
// It sets the status to "running" in the state store, launches the discovery
// container in the background, and updates the status on completion/failure.
// Returns ErrDiscoveryAlreadyRunning if a discovery is already in progress.
func (d *Agent) RunDiscoveryAsync(ctx context.Context, job *api.RenovateJob) error {
	// Secondary guard: check state store for an existing "running" status
	ds, err := d.store.GetDiscoveryStatus(ctx, job.Name)
	if err != nil {
		return fmt.Errorf("check discovery status: %w", err)
	}
	if ds != nil && ds.Status == "running" {
		return executor.ErrDiscoveryAlreadyRunning
	}

	// Set status to running
	now := time.Now().UTC()
	if err := d.store.SetDiscoveryStatus(ctx, job.Name, "running", &now, nil, ""); err != nil {
		return fmt.Errorf("set discovery running status: %w", err)
	}

	// Launch async dispatch with a callback that reconciles projects and updates status
	err = d.executor.DispatchDiscoveryAsync(ctx, job, func(repos []string, dispatchErr error) {
		completedAt := time.Now().UTC()

		if dispatchErr != nil {
			d.logger.Error("async discovery failed", "job", job.Name, "error", dispatchErr)
			_ = d.store.SetDiscoveryStatus(context.Background(), job.Name, "failed", &now, &completedAt, dispatchErr.Error())
			return
		}

		d.logger.Info("async discovery found repos", "job", job.Name, "count", len(repos))

		// Reconcile projects BEFORE setting status to completed (prevents race)
		removed, reconcileErr := d.store.ReconcileProjects(context.Background(), job, repos)
		if reconcileErr != nil {
			d.logger.Error("async discovery reconcile failed", "job", job.Name, "error", reconcileErr)
			_ = d.store.SetDiscoveryStatus(context.Background(), job.Name, "failed", &now, &completedAt, reconcileErr.Error())
			return
		}

		if len(removed) > 0 {
			d.logger.Info("projects removed during async reconciliation",
				"job", job.Name, "removed", removed)
		}

		_ = d.store.SetDiscoveryStatus(context.Background(), job.Name, "completed", &now, &completedAt, "")
		d.logger.Info("async discovery completed", "job", job.Name, "repos", len(repos))
	})
	if err != nil {
		// If dispatch failed (e.g., already running), revert status
		_ = d.store.SetDiscoveryStatus(ctx, job.Name, "idle", nil, nil, "")
		return err
	}

	return nil
}
