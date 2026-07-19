// Package executor runs Renovate as Docker containers instead of Kubernetes Jobs.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/parser"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/statestore"
)

const (
	labelProject   = "renovate-standalone/project"
	labelJobName   = "renovate-standalone/job-name"
	labelType      = "renovate-standalone/type"
	labelStartedAt = "renovate-standalone/started-at"

	typeExecutor  = "executor"
	typeDiscovery = "discovery"
)

// Config holds configuration for the DockerExecutor.
type Config struct {
	Image           string
	Network         string
	CacheVolume     string
	Parallelism     int
	JobTimeout      time.Duration
	GracePeriod     time.Duration
	ImagePullPolicy string // "always", "if-not-present", "never"
	ImageCacheTTL   time.Duration
}

// ErrDiscoveryAlreadyRunning is returned when a discovery container is already
// tracked for the same job.
var ErrDiscoveryAlreadyRunning = errors.New("discovery is already running for this job")

// DockerExecutor manages Renovate container lifecycle via the Docker API.
type DockerExecutor struct {
	docker *client.Client
	store  statestore.RenovateJobManager
	logger *slog.Logger

	// Configuration
	image           string
	network         string
	cacheVolume     string
	parallelism     int
	jobTimeout      time.Duration
	gracePeriod     time.Duration
	imagePullPolicy string
	imageCacheTTL   time.Duration

	// Runtime state
	mu         sync.Mutex
	running    map[string]string    // project → containerID
	imageCache map[string]time.Time // image tag → last-checked time
	stopCh     chan struct{}

	// Discovery tracking
	discoveryMu sync.Mutex
	discoveryID map[string]string // jobName → containerID
}

// New creates a new DockerExecutor with the given configuration.
func New(cfg Config, store statestore.RenovateJobManager, logger *slog.Logger) (*DockerExecutor, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	if logger == nil {
		logger = slog.Default()
	}

	// Apply defaults
	if cfg.Image == "" {
		cfg.Image = "renovate/renovate:latest"
	}
	if cfg.Network == "" {
		cfg.Network = "bridge"
	}
	if cfg.CacheVolume == "" {
		cfg.CacheVolume = "renovate-cache"
	}
	if cfg.Parallelism <= 0 {
		cfg.Parallelism = 2
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = 1800 * time.Second
	}
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = 300 * time.Second
	}
	if cfg.ImagePullPolicy == "" {
		cfg.ImagePullPolicy = "if-not-present"
	}

	return &DockerExecutor{
		docker:          cli,
		store:           store,
		logger:          logger,
		image:           cfg.Image,
		network:         cfg.Network,
		cacheVolume:     cfg.CacheVolume,
		parallelism:     cfg.Parallelism,
		jobTimeout:      cfg.JobTimeout,
		gracePeriod:     cfg.GracePeriod,
		imagePullPolicy: cfg.ImagePullPolicy,
		imageCacheTTL:   cfg.ImageCacheTTL,
		running:         make(map[string]string),
		imageCache:      make(map[string]time.Time),
		stopCh:          make(chan struct{}),
		discoveryID:     make(map[string]string),
	}, nil
}

// Ping verifies connectivity to the Docker daemon.
func (e *DockerExecutor) Ping(ctx context.Context) error {
	_, err := e.docker.Ping(ctx)
	if err != nil {
		return fmt.Errorf("docker ping: %w", err)
	}
	return nil
}

// Start begins the executor event loop. It listens for Docker events, dispatches
// scheduled jobs, and runs periodic orphan reconciliation.
func (e *DockerExecutor) Start(ctx context.Context) error {
	e.logger.Info("starting docker executor",
		"parallelism", e.parallelism,
		"image", e.image,
		"network", e.network,
	)

	// Channel to signal a re-dispatch check
	dispatch := make(chan struct{}, 1)
	triggerDispatch := func() {
		select {
		case dispatch <- struct{}{}:
		default:
		}
	}

	// Start Docker events listener
	go e.listenEvents(ctx, triggerDispatch)

	// Start main dispatch loop
	go e.dispatchLoop(ctx, dispatch)

	// Start orphan reconciliation loop
	go e.reconcileLoop(ctx)

	return nil
}

// Stop gracefully shuts down the executor. It signals running containers to
// stop and waits for the grace period before killing them.
func (e *DockerExecutor) Stop() error {
	close(e.stopCh)

	e.mu.Lock()
	containers := make(map[string]string, len(e.running))
	for proj, cid := range e.running {
		containers[proj] = cid
	}
	e.mu.Unlock()

	if len(containers) == 0 {
		if err := e.docker.Close(); err != nil {
			e.logger.Error("error closing docker client", "error", err)
		}
		return nil
	}

	e.logger.Info("stopping running containers", "count", len(containers))

	ctx := context.Background()
	graceSec := int(e.gracePeriod.Seconds())

	for project, containerID := range containers {
		e.logger.Info("stopping container", "project", project, "container", containerID)
		stopOpts := container.StopOptions{Timeout: &graceSec}
		if err := e.docker.ContainerStop(ctx, containerID, stopOpts); err != nil {
			e.logger.Warn("failed to stop container, killing",
				"container", containerID, "error", err)
			_ = e.docker.ContainerKill(ctx, containerID, "SIGKILL")
		}

		e.mu.Lock()
		delete(e.running, project)
		e.mu.Unlock()
	}

	if err := e.docker.Close(); err != nil {
		e.logger.Error("error closing docker client", "error", err)
	}

	return nil
}

// DispatchDiscovery runs an autodiscovery container for the given job and
// returns the list of discovered repositories.
func (e *DockerExecutor) DispatchDiscovery(ctx context.Context, job *api.RenovateJob) ([]string, error) {
	e.logger.Info("running discovery", "job", job.Name)

	if err := e.pullImage(ctx); err != nil {
		return nil, fmt.Errorf("pull image for discovery: %w", err)
	}

	envVars := e.buildEnvVars(job, true)

	discoveryCmd := `BASE_DIR="${RENOVATE_BASE_DIR:-/tmp}"; renovate --autodiscover --write-discovered-repos "$BASE_DIR/repos.json" >> "$BASE_DIR/logs.json" 2>&1 && cat "$BASE_DIR/repos.json" || cat "$BASE_DIR/logs.json"`

	labels := map[string]string{
		labelJobName:   job.Name,
		labelType:      typeDiscovery,
		labelStartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	containerCfg := &container.Config{
		Image:  e.image,
		Cmd:    []string{"/bin/sh", "-c", discoveryCmd},
		Env:    envVars,
		Labels: labels,
		User:   "12021:12021",
	}

	hostCfg := &container.HostConfig{
		Binds:       []string{e.cacheVolume + ":/tmp/renovate"},
		NetworkMode: container.NetworkMode(e.network),
	}

	name := fmt.Sprintf("renovate-discovery-%s-%d", sanitizeName(job.Name), time.Now().Unix())

	resp, err := e.docker.ContainerCreate(ctx, containerCfg, hostCfg, &network.NetworkingConfig{}, nil, name)
	if err != nil {
		return nil, fmt.Errorf("create discovery container: %w", err)
	}
	containerID := resp.ID

	if err := e.docker.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start discovery container: %w", err)
	}

	// Wait for the container to finish
	statusCh, errCh := e.docker.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			return nil, fmt.Errorf("wait for discovery container: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			// Read logs for error info
			logs, _ := e.getContainerLogs(ctx, containerID)
			_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			return nil, fmt.Errorf("discovery container exited with code %d: %s", status.StatusCode, string(logs))
		}
	case <-ctx.Done():
		_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return nil, ctx.Err()
	}

	// Read stdout from the container logs
	output, err := e.getContainerLogs(ctx, containerID)
	if err != nil {
		_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("read discovery logs: %w", err)
	}

	_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})

	// Parse JSON array of repo names
	var repos []string
	if err := json.Unmarshal(output, &repos); err != nil {
		return nil, fmt.Errorf("parse discovery output: %w (raw: %s)", err, string(output))
	}

	e.logger.Info("discovery completed", "job", job.Name, "repos", len(repos))
	return repos, nil
}

// DispatchDiscoveryAsync starts a discovery container in a background goroutine.
// It returns immediately after starting the container. The callback is invoked
// with the discovered repos (or error) once the container exits.
// Returns ErrDiscoveryAlreadyRunning if a discovery container is already tracked for the job.
func (e *DockerExecutor) DispatchDiscoveryAsync(_ context.Context, job *api.RenovateJob, callback func([]string, error)) error {
	e.discoveryMu.Lock()
	if _, running := e.discoveryID[job.Name]; running {
		e.discoveryMu.Unlock()
		return ErrDiscoveryAlreadyRunning
	}
	// Reserve the slot (will be updated with real container ID after start)
	e.discoveryID[job.Name] = ""
	e.discoveryMu.Unlock()

	// Use a background context derived from stopCh so the goroutine outlives the HTTP request
	bgCtx, bgCancel := context.WithCancel(context.Background())
	go func() {
		defer bgCancel()
		defer func() {
			e.discoveryMu.Lock()
			delete(e.discoveryID, job.Name)
			e.discoveryMu.Unlock()
		}()

		// Abort if the executor is stopping
		go func() {
			select {
			case <-e.stopCh:
				bgCancel()
			case <-bgCtx.Done():
			}
		}()

		repos, err := e.runDiscoveryContainer(bgCtx, job)
		if callback != nil {
			callback(repos, err)
		}
	}()

	return nil
}

// GetRunningDiscoveryContainerID returns the container ID for a running discovery, if any.
func (e *DockerExecutor) GetRunningDiscoveryContainerID(jobName string) (string, bool) {
	e.discoveryMu.Lock()
	defer e.discoveryMu.Unlock()
	cid, ok := e.discoveryID[jobName]
	if !ok || cid == "" {
		return "", false
	}
	return cid, true
}

// runDiscoveryContainer runs the discovery container synchronously and returns repos.
// It registers/unregisters the container ID in the discovery tracking map.
func (e *DockerExecutor) runDiscoveryContainer(ctx context.Context, job *api.RenovateJob) ([]string, error) {
	e.logger.Info("running async discovery", "job", job.Name)

	if err := e.pullImage(ctx); err != nil {
		return nil, fmt.Errorf("pull image for discovery: %w", err)
	}

	envVars := e.buildEnvVars(job, true)

	discoveryCmd := `BASE_DIR="${RENOVATE_BASE_DIR:-/tmp}"; renovate --autodiscover --write-discovered-repos "$BASE_DIR/repos.json" >> "$BASE_DIR/logs.json" 2>&1 && cat "$BASE_DIR/repos.json" || cat "$BASE_DIR/logs.json"`

	labels := map[string]string{
		labelJobName:   job.Name,
		labelType:      typeDiscovery,
		labelStartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	containerCfg := &container.Config{
		Image:  e.image,
		Cmd:    []string{"/bin/sh", "-c", discoveryCmd},
		Env:    envVars,
		Labels: labels,
		User:   "12021:12021",
	}

	hostCfg := &container.HostConfig{
		Binds:       []string{e.cacheVolume + ":/tmp/renovate"},
		NetworkMode: container.NetworkMode(e.network),
	}

	name := fmt.Sprintf("renovate-discovery-%s-%d", sanitizeName(job.Name), time.Now().Unix())

	resp, err := e.docker.ContainerCreate(ctx, containerCfg, hostCfg, &network.NetworkingConfig{}, nil, name)
	if err != nil {
		return nil, fmt.Errorf("create discovery container: %w", err)
	}
	containerID := resp.ID

	if err := e.docker.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start discovery container: %w", err)
	}

	// Register the real container ID for tracking
	e.discoveryMu.Lock()
	e.discoveryID[job.Name] = containerID
	e.discoveryMu.Unlock()

	// Wait for the container to finish
	statusCh, errCh := e.docker.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			return nil, fmt.Errorf("wait for discovery container: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			logs, _ := e.getContainerLogs(ctx, containerID)
			_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			return nil, fmt.Errorf("discovery container exited with code %d: %s", status.StatusCode, string(logs))
		}
	case <-ctx.Done():
		_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return nil, ctx.Err()
	}

	// Read stdout from the container logs
	output, err := e.getContainerLogs(ctx, containerID)
	if err != nil {
		_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("read discovery logs: %w", err)
	}

	_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})

	// Parse JSON array of repo names
	var repos []string
	if err := json.Unmarshal(output, &repos); err != nil {
		return nil, fmt.Errorf("parse discovery output: %w (raw: %s)", err, string(output))
	}

	e.logger.Info("async discovery completed", "job", job.Name, "repos", len(repos))
	return repos, nil
}

// dispatchLoop periodically checks for scheduled jobs and dispatches them.
func (e *DockerExecutor) dispatchLoop(ctx context.Context, dispatch <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.doDispatch(ctx)
		case <-dispatch:
			e.doDispatch(ctx)
		}
	}
}

// doDispatch finds scheduled jobs and starts containers up to the parallelism limit.
func (e *DockerExecutor) doDispatch(ctx context.Context) {
	jobs, err := e.store.ListRenovateJobsFull(ctx)
	if err != nil {
		e.logger.Error("failed to list jobs for dispatch", "error", err)
		return
	}

	e.mu.Lock()
	runningCount := len(e.running)
	e.mu.Unlock()

	for i := range jobs {
		if runningCount >= e.parallelism {
			break
		}

		job := &jobs[i]
		jobID := statestore.RenovateJobIdentifier{Name: job.Name}

		scheduled, err := e.store.GetProjectsByStatus(ctx, jobID, api.JobStatusScheduled)
		if err != nil {
			e.logger.Error("failed to get scheduled projects", "job", job.Name, "error", err)
			continue
		}

		// Sort by priority (higher priority first)
		sort.Slice(scheduled, func(a, b int) bool {
			return scheduled[a].Priority > scheduled[b].Priority
		})

		for _, proj := range scheduled {
			if runningCount >= e.parallelism {
				break
			}

			// Reserve the slot under the mutex before dispatching
			e.mu.Lock()
			if _, alreadyRunning := e.running[proj.Name]; alreadyRunning {
				e.mu.Unlock()
				continue
			}
			// Reserve slot with empty container ID (will be filled by dispatchProject)
			e.running[proj.Name] = ""
			e.mu.Unlock()

			if err := e.dispatchProject(ctx, job, proj.Name); err != nil {
				e.logger.Error("failed to dispatch project",
					"project", proj.Name, "job", job.Name, "error", err)
				// Release the reserved slot on failure
				e.mu.Lock()
				delete(e.running, proj.Name)
				e.mu.Unlock()
				continue
			}
			runningCount++
		}
	}
}

// dispatchProject creates and starts a container for a single project.
func (e *DockerExecutor) dispatchProject(ctx context.Context, job *api.RenovateJob, project string) error {
	e.logger.Info("dispatching project", "project", project, "job", job.Name)

	if err := e.pullImage(ctx); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	envVars := e.buildEnvVars(job, false)
	now := time.Now().UTC()

	labels := map[string]string{
		labelProject:   project,
		labelJobName:   job.Name,
		labelType:      typeExecutor,
		labelStartedAt: now.Format(time.RFC3339),
	}

	containerCfg := &container.Config{
		Image:  e.image,
		Cmd:    []string{"renovate", project},
		Env:    envVars,
		Labels: labels,
		User:   "12021:12021",
	}

	hostCfg := &container.HostConfig{
		Binds:       []string{e.cacheVolume + ":/tmp/renovate"},
		NetworkMode: container.NetworkMode(e.network),
	}

	name := fmt.Sprintf("renovate-%s-%d", sanitizeName(project), now.Unix())

	resp, err := e.docker.ContainerCreate(ctx, containerCfg, hostCfg, &network.NetworkingConfig{}, nil, name)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	containerID := resp.ID

	if err := e.docker.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return fmt.Errorf("start container: %w", err)
	}

	// Immediately update state store and running map after successful start
	// to minimize the orphan window.
	e.mu.Lock()
	e.running[project] = containerID
	e.mu.Unlock()

	jobID := statestore.RenovateJobIdentifier{Name: job.Name}
	if err := e.store.UpdateProjectStatus(ctx, project, jobID, &statestore.RenovateStatusUpdate{
		Status: api.JobStatusRunning,
	}); err != nil {
		e.logger.Error("failed to update project status to running",
			"project", project, "error", err)
	}

	e.logger.Info("container started", "project", project, "container", containerID)
	return nil
}

// listenEvents subscribes to Docker container events and handles die events.
func (e *DockerExecutor) listenEvents(ctx context.Context, triggerDispatch func()) {
	f := filters.NewArgs()
	f.Add("type", string(events.ContainerEventType))
	f.Add("event", "die")
	f.Add("label", labelType)

	eventsCh, errCh := e.docker.Events(ctx, events.ListOptions{Filters: f})

	// Limit concurrent exit-handling goroutines
	exitPool := make(chan struct{}, max(e.parallelism*2, 8))

	for {
		select {
		case <-e.stopCh:
			return
		case <-ctx.Done():
			return
		case err := <-errCh:
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				e.logger.Error("docker events error, reconnecting", "error", err)
				time.Sleep(5 * time.Second)
				if ctx.Err() != nil {
					return
				}
				eventsCh, errCh = e.docker.Events(ctx, events.ListOptions{Filters: f})
			}
		case event := <-eventsCh:
			if event.Action == "die" {
				exitPool <- struct{}{}
				go func(containerID string) {
					defer func() { <-exitPool }()
					e.handleContainerExit(ctx, containerID)
					triggerDispatch()
				}(event.Actor.ID)
			}
		}
	}
}

// handleContainerExit processes a container that has exited.
func (e *DockerExecutor) handleContainerExit(ctx context.Context, containerID string) {
	// Find the project associated with this container
	e.mu.Lock()
	var project string
	for proj, cid := range e.running {
		if cid == containerID {
			project = proj
			break
		}
	}
	e.mu.Unlock()

	if project == "" {
		// Try to get project from container labels
		info, err := e.docker.ContainerInspect(ctx, containerID)
		if err != nil {
			e.logger.Warn("cannot inspect exited container", "container", containerID, "error", err)
			return
		}
		project = info.Config.Labels[labelProject]
		if project == "" {
			// Discovery container or unknown — skip
			return
		}
	}

	// Get container info for exit code and job name
	info, err := e.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		e.logger.Error("failed to inspect container", "container", containerID, "error", err)
		e.mu.Lock()
		delete(e.running, project)
		e.mu.Unlock()
		return
	}

	exitCode := 0
	if info.State != nil {
		exitCode = info.State.ExitCode
	}

	jobName := info.Config.Labels[labelJobName]
	startedAtStr := info.Config.Labels[labelStartedAt]

	// Calculate duration
	var duration *string
	if startedAtStr != "" {
		if startedAt, err := time.Parse(time.RFC3339, startedAtStr); err == nil {
			d := time.Since(startedAt).Round(time.Second).String()
			duration = &d
		}
	}

	// Collect logs
	logs, err := e.getContainerLogs(ctx, containerID)
	if err != nil {
		e.logger.Warn("failed to collect container logs", "container", containerID, "error", err)
	}
	// Store and parse logs
	var parseResult *parser.LogParseResult
	if len(logs) > 0 && jobName != "" {
		jobID := statestore.RenovateJobIdentifier{Name: jobName}
		if err := e.store.StoreProjectLogs(ctx, jobID, project, logs); err != nil {
			e.logger.Error("failed to store project logs", "project", project, "error", err)
		}
		parseResult = parser.ParseRenovateLogs(string(logs))
	}

	// Determine final status
	status := api.JobStatusCompleted
	if exitCode != 0 {
		status = api.JobStatusFailed
	}

	e.logger.Info("container exited",
		"project", project,
		"container", containerID,
		"exitCode", exitCode,
		"status", status,
	)

	// Update state store
	if jobName != "" {
		jobID := statestore.RenovateJobIdentifier{Name: jobName}
		statusUpdate := &statestore.RenovateStatusUpdate{
			Status:   status,
			Duration: duration,
		}
		if parseResult != nil {
			statusUpdate.RenovateResultStatus = parseResult.RenovateResultStatus
			statusUpdate.PRActivity = parseResult.PRActivity
			statusUpdate.LogIssues = parseResult.LogIssues
		}
		if err := e.store.UpdateProjectStatus(ctx, project, jobID, statusUpdate); err != nil {
			e.logger.Error("failed to update project status after exit",
				"project", project, "error", err)
		}
	}

	// Remove from running map
	e.mu.Lock()
	delete(e.running, project)
	e.mu.Unlock()

	// Remove the container
	if err := e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		e.logger.Warn("failed to remove container", "container", containerID, "error", err)
	}
}

// reconcileLoop periodically checks for orphaned containers.
func (e *DockerExecutor) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.reconcileOrphans(ctx); err != nil {
				e.logger.Error("orphan reconciliation failed", "error", err)
			}
		}
	}
}

// reconcileOrphans finds containers with our labels that aren't tracked in the running map.
func (e *DockerExecutor) reconcileOrphans(ctx context.Context) error {
	f := filters.NewArgs()
	f.Add("label", labelType+"="+typeExecutor)

	containers, err := e.docker.ContainerList(ctx, container.ListOptions{Filters: f, All: true})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	e.mu.Lock()
	tracked := make(map[string]bool, len(e.running))
	for _, cid := range e.running {
		tracked[cid] = true
	}
	e.mu.Unlock()

	for _, c := range containers {
		if tracked[c.ID] {
			continue
		}

		project := c.Labels[labelProject]
		e.logger.Warn("found orphaned container",
			"container", c.ID[:12],
			"project", project,
			"state", c.State,
		)

		// If the container is still running, check timeout
		if c.State == "running" {
			startedAtStr := c.Labels[labelStartedAt]
			if startedAtStr != "" {
				if startedAt, err := time.Parse(time.RFC3339, startedAtStr); err == nil {
					if time.Since(startedAt) > e.jobTimeout {
						e.logger.Warn("killing timed-out orphan container",
							"container", c.ID[:12], "project", project)
						_ = e.docker.ContainerKill(ctx, c.ID, "SIGKILL")
					} else {
						// Re-adopt the container
						if project != "" {
							e.mu.Lock()
							e.running[project] = c.ID
							e.mu.Unlock()

							// Sync state store so the project reflects running status
							jobName := c.Labels[labelJobName]
							if jobName != "" {
								jobID := statestore.RenovateJobIdentifier{Name: jobName}
								if err := e.store.UpdateProjectStatus(ctx, project, jobID, &statestore.RenovateStatusUpdate{
									Status: api.JobStatusRunning,
								}); err != nil {
									e.logger.Error("failed to update state store after re-adoption",
										"project", project, "error", err)
								}
							}

							e.logger.Info("re-adopted orphan container",
								"container", c.ID[:12], "project", project)
						}
						continue
					}
				}
			}
		}

		// For stopped/dead orphans, handle exit and clean up
		if c.State == "exited" || c.State == "dead" {
			e.handleContainerExit(ctx, c.ID)
		}
	}

	return nil
}

// pullImage pulls the configured image based on the image pull policy.
func (e *DockerExecutor) pullImage(ctx context.Context) error {
	switch e.imagePullPolicy {
	case "never":
		return nil
	case "if-not-present":
		// Check image cache first (if TTL > 0)
		if e.imageCacheTTL > 0 {
			e.mu.Lock()
			if lastCheck, ok := e.imageCache[e.image]; ok && time.Since(lastCheck) < e.imageCacheTTL {
				e.mu.Unlock()
				return nil
			}
			e.mu.Unlock()
		}
		// Check if image exists locally
		_, err := e.docker.ImageInspect(ctx, e.image)
		if err == nil {
			// Image present — update cache
			if e.imageCacheTTL > 0 {
				e.mu.Lock()
				e.imageCache[e.image] = time.Now()
				e.mu.Unlock()
			}
			return nil // Already present
		}
	case "always":
		// Always pull
	default:
		// Default to if-not-present behavior
		if e.imageCacheTTL > 0 {
			e.mu.Lock()
			if lastCheck, ok := e.imageCache[e.image]; ok && time.Since(lastCheck) < e.imageCacheTTL {
				e.mu.Unlock()
				return nil
			}
			e.mu.Unlock()
		}
		_, err := e.docker.ImageInspect(ctx, e.image)
		if err == nil {
			if e.imageCacheTTL > 0 {
				e.mu.Lock()
				e.imageCache[e.image] = time.Now()
				e.mu.Unlock()
			}
			return nil
		}
	}

	e.logger.Info("pulling image", "image", e.image)
	reader, err := e.docker.ImagePull(ctx, e.image, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", e.image, err)
	}
	defer func() { _ = reader.Close() }()
	// Drain the reader to complete the pull
	_, _ = io.Copy(io.Discard, reader)

	// Update cache after successful pull
	if e.imageCacheTTL > 0 {
		e.mu.Lock()
		e.imageCache[e.image] = time.Now()
		e.mu.Unlock()
	}
	return nil
}

// buildEnvVars constructs environment variables for the Renovate container.
// Priority chain: protected defaults < overridable defaults + job spec + DB toggle
//
//	< RENOVATE_* passthrough (operator env) < ExtraEnv (per-job UI)
func (e *DockerExecutor) buildEnvVars(job *api.RenovateJob, isDiscovery bool) []string {
	envMap := make(map[string]string)

	// Overridable defaults
	envMap["RENOVATE_LOG_FORMAT"] = "json"
	envMap["NODE_NO_WARNINGS"] = "1"
	envMap["RENOVATE_BASE_DIR"] = "/tmp/renovate"

	if job.Spec.Provider != nil {
		if job.Spec.Provider.Name != "" {
			envMap["RENOVATE_PLATFORM"] = job.Spec.Provider.Name
		}
		if job.Spec.Provider.Endpoint != "" {
			envMap["RENOVATE_ENDPOINT"] = job.Spec.Provider.Endpoint
		}
	}

	// Log level from DB toggle: default to debug (required for complete PR activity parsing).
	if job.Status.ExecutionOptions != nil && !job.Status.ExecutionOptions.Debug {
		envMap["RENOVATE_LOG_LEVEL"] = "info"
	} else {
		envMap["RENOVATE_LOG_LEVEL"] = "debug"
	}

	// Discovery-specific env vars
	if isDiscovery {
		if len(job.Spec.DiscoveryFilters) > 0 {
			envMap["RENOVATE_AUTODISCOVER_FILTER"] = strings.Join(job.Spec.DiscoveryFilters, ",")
		}
		if len(job.Spec.DiscoverTopics) > 0 {
			envMap["RENOVATE_AUTODISCOVER_TOPICS"] = strings.Join(job.Spec.DiscoverTopics, ",")
		}
	}

	// Pass through all RENOVATE_* environment variables from the operator process.
	// Always overrides defaults, job spec values, and DB settings.
	for _, env := range os.Environ() {
		key, value, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "RENOVATE_") && value != "" {
			envMap[key] = value
		}
	}

	// Re-enforce protected hardcoded defaults that must not be overridden by passthrough.
	protectedDefaults := map[string]string{
		"RENOVATE_LOG_FORMAT": "json",
		"RENOVATE_BASE_DIR":   "/tmp/renovate",
	}
	for k, v := range protectedDefaults {
		if envMap[k] != v {
			slog.Warn("ignoring env override for protected container envvar", "key", k, "forced", v)
			envMap[k] = v
		}
	}

	// Apply user ExtraEnv overrides (these take priority over everything)
	for _, env := range job.Spec.ExtraEnv {
		envMap[env.Name] = env.Value
	}

	// Convert map to sorted slice
	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := make([]string, 0, len(keys))
	for _, k := range keys {
		result = append(result, k+"="+envMap[k])
	}
	return result
}

// getContainerLogs retrieves all logs from a container.
func (e *DockerExecutor) getContainerLogs(ctx context.Context, containerID string) ([]byte, error) {
	reader, err := e.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     false,
		Timestamps: false,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()

	// Read the full stream into memory.
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	// Docker multiplexes stdout/stderr with 8-byte frame headers when Tty is
	// not set. Podman with json-file log driver returns raw text.
	// Detect multiplexed format by checking the first 8 bytes.
	if isDockerMultiplexed(raw) {
		var stdout, stderr bytes.Buffer
		if _, err := stdcopy.StdCopy(&stdout, &stderr, bytes.NewReader(raw)); err != nil {
			return nil, err
		}
		return stdout.Bytes(), nil
	}

	// Raw stream (Podman or TTY mode) — return as-is.
	return raw, nil
}

// isDockerMultiplexed checks whether the byte slice starts with a valid Docker
// stream frame header. The header format is:
//   - byte 0: stream type (0x01=stdout, 0x02=stderr)
//   - bytes 1-3: padding (must be 0x00)
//   - bytes 4-7: big-endian uint32 payload size
func isDockerMultiplexed(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	// Stream type must be stdout (1) or stderr (2)
	if data[0] != 0x01 && data[0] != 0x02 {
		return false
	}
	// Padding bytes must be zero
	if data[1] != 0x00 || data[2] != 0x00 || data[3] != 0x00 {
		return false
	}
	// Payload length must be reasonable (not exceed total buffer minus header)
	payloadLen := uint32(data[4])<<24 | uint32(data[5])<<16 | uint32(data[6])<<8 | uint32(data[7])
	if payloadLen == 0 || payloadLen > uint32(len(data)-8) {
		return false
	}
	return true
}

// GetRunningContainerID returns the container ID for a running project, if any.
func (e *DockerExecutor) GetRunningContainerID(project string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cid, ok := e.running[project]
	return cid, ok
}

// StreamContainerLogs returns a live stream of container logs with stdout and
// stderr demultiplexed. Stderr lines that are plain text (not valid NDJSON) are
// wrapped as JSON log entries before writing. The returned reader blocks until
// the container exits or the context is cancelled.
func (e *DockerExecutor) StreamContainerLogs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	reader, err := e.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: false,
	})
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()

	go func() {
		defer func() { _ = reader.Close() }()

		// stderrWriter wraps non-JSON stderr lines as NDJSON.
		stderrWriter := &stderrJSONWrapper{w: pw}

		// StdCopy processes frames sequentially: stdout frames write directly
		// to pw, stderr frames go through stderrWriter. No concurrent writes.
		_, err := stdcopy.StdCopy(pw, stderrWriter, reader)
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}()

	return pr, nil
}

// stderrJSONWrapper is an io.Writer that ensures each line written is valid
// NDJSON. If a line is already valid JSON it passes through unchanged.
// Plain-text lines are wrapped as {"level":60,"msg":"[stderr] ...","time":"..."}.
type stderrJSONWrapper struct {
	w   io.Writer
	buf []byte
}

func (s *stderrJSONWrapper) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		nl := bytes.IndexByte(s.buf, '\n')
		if nl < 0 {
			break
		}
		line := s.buf[:nl]
		s.buf = s.buf[nl+1:]

		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}

		if json.Valid(trimmed) {
			// Already valid JSON — pass through.
			if _, err := s.w.Write(append(trimmed, '\n')); err != nil {
				return len(p), err
			}
		} else {
			// Wrap plain text as JSON log entry.
			entry := map[string]interface{}{
				"level": 60,
				"msg":   "[stderr] " + string(trimmed),
				"time":  time.Now().UTC().Format(time.RFC3339Nano),
			}
			data, _ := json.Marshal(entry)
			if _, err := s.w.Write(append(data, '\n')); err != nil {
				return len(p), err
			}
		}
	}
	return len(p), nil
}

// StopContainer stops a specific container by project name.
func (e *DockerExecutor) StopContainer(ctx context.Context, project string) error {
	e.mu.Lock()
	containerID, ok := e.running[project]
	e.mu.Unlock()

	if !ok {
		return fmt.Errorf("no running container for project %s", project)
	}

	graceSec := int(e.gracePeriod.Seconds())
	stopOpts := container.StopOptions{Timeout: &graceSec}
	return e.docker.ContainerStop(ctx, containerID, stopOpts)
}

// sanitizeName converts a project name to a valid Docker container name component.
func sanitizeName(name string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		".", "-",
		":", "-",
		" ", "-",
	)
	result := replacer.Replace(strings.ToLower(name))
	// Trim to reasonable length
	if len(result) > 60 {
		result = result[:60]
	}
	return result
}
