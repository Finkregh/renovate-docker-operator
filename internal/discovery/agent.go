// Package discovery handles project autodiscovery from git platforms.
// Runs the Renovate discovery container and parses its output.
package discovery

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/oluf-tech/renovate-docker-operator/internal/api"
	"github.com/oluf-tech/renovate-docker-operator/internal/executor"
	"github.com/oluf-tech/renovate-docker-operator/internal/statestore"
)

// DiscoveryAgent manages project discovery for RenovateJobs.
type DiscoveryAgent struct {
	executor *executor.DockerExecutor
	store    statestore.RenovateJobManager
	logger   *slog.Logger
}

// New creates a new DiscoveryAgent.
func New(exec *executor.DockerExecutor, store statestore.RenovateJobManager, logger *slog.Logger) *DiscoveryAgent {
	if logger == nil {
		logger = slog.Default()
	}
	return &DiscoveryAgent{
		executor: exec,
		store:    store,
		logger:   logger,
	}
}

// RunDiscovery runs autodiscovery for the given job and reconciles projects.
// Returns the list of removed projects (for webhook cleanup).
func (d *DiscoveryAgent) RunDiscovery(ctx context.Context, job *api.RenovateJob) (removed []string, err error) {
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
