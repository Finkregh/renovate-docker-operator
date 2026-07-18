// Package server provides the unified HTTP server for renovate-docker-operator.
// It combines the UI, webhook, health, and API endpoints on a single port.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/config"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/discovery"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/scheduler"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/statestore"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/webhook"
)

// Server is the unified HTTP server that handles UI, webhook, health and API.
type Server struct {
	store     statestore.RenovateJobManager
	discovery *discovery.Agent
	scheduler *scheduler.Scheduler
	webhook   *webhook.Handler
	logger    *slog.Logger
	server    *http.Server
	version   string
}

// Config holds server configuration.
type Config struct {
	Port    string
	Version string
}

// New creates a new unified server.
func New(
	store statestore.RenovateJobManager,
	disc *discovery.Agent,
	sched *scheduler.Scheduler,
	logger *slog.Logger,
	version string,
) *Server {
	wh := webhook.NewHandler(store, logger)
	return &Server{
		store:     store,
		discovery: disc,
		scheduler: sched,
		webhook:   wh,
		logger:    logger,
		version:   version,
	}
}

// Start begins listening on the configured port. Non-blocking.
func (s *Server) Start() {
	router := mux.NewRouter()

	// Health endpoints (no auth)
	router.HandleFunc("/healthz", s.healthHandler).Methods("GET")
	router.HandleFunc("/readyz", s.healthHandler).Methods("GET")

	// Webhook routes
	whSub := router.PathPrefix("/webhook/v1").Subrouter()
	whSub.HandleFunc("/forgejo", s.webhook.HandleForgejo).Methods("POST")
	whSub.HandleFunc("/schedule", s.webhook.HandleSchedule).Methods("POST")

	// API v1 routes
	apiSub := router.PathPrefix("/api/v1").Subrouter()
	apiSub.HandleFunc("/version", s.getVersion).Methods("GET")
	apiSub.HandleFunc("/renovatejobs", s.getRenovateJobs).Methods("GET")
	apiSub.HandleFunc("/renovate", s.runRenovateForProject).Methods("POST")
	apiSub.HandleFunc("/renovate/all", s.runRenovateForAllProjects).Methods("POST")
	apiSub.HandleFunc("/renovate/cancel", s.cancelRenovateForProject).Methods("POST")
	apiSub.HandleFunc("/logs", s.getRenovateJobLogs).Methods("GET")
	apiSub.HandleFunc("/discovery/start", s.runDiscovery).Methods("POST")
	apiSub.HandleFunc("/executionOptions", s.updateExecutionOptions).Methods("POST")

	// UI static file serving (last — catch-all)
	s.registerUIRoutes(router)

	port := config.GetValue("SERVER_PORT")
	if port == "" {
		port = "8081"
	}

	s.server = &http.Server{
		Addr:         ":" + port,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		s.logger.Info("starting HTTP server", "port", port)
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP server error", "error", err)
		}
	}()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// --- Health ---

func (s *Server) healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// --- API v1 ---

// RenovateJobInfo is the JSON response for listing jobs.
type RenovateJobInfo struct {
	Name             string                             `json:"name"`
	CronExpression   string                             `json:"cronExpression"`
	NextSchedule     time.Time                          `json:"nextSchedule"`
	Projects         []statestore.RenovateProjectStatus `json:"projects"`
	Platform         string                             `json:"platform,omitempty"`
	PlatformEndpoint string                             `json:"platformEndpoint,omitempty"`
	ExecutionOptions *api.RenovateExecutionOptions      `json:"executionOptions,omitempty"`
}

func (s *Server) getVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.version})
}

func (s *Server) getRenovateJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListRenovateJobsFull(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load jobs"})
		return
	}

	result := make([]RenovateJobInfo, 0, len(jobs))
	for i := range jobs {
		job := &jobs[i]

		var platform, endpoint string
		if job.Spec.Provider != nil {
			platform = job.Spec.Provider.Name
			endpoint = job.Spec.Provider.Endpoint
		}

		projects := make([]statestore.RenovateProjectStatus, 0, len(job.Status.Projects))
		for _, p := range job.Status.Projects {
			projects = append(projects, statestore.RenovateProjectStatus{
				Name:                 p.Name,
				Status:               p.Status,
				LastRun:              p.LastRun,
				Priority:             p.Priority,
				RenovateResultStatus: p.RenovateResultStatus,
				Duration:             p.Duration,
				PRActivity:           p.PRActivity,
				LogIssues:            p.LogIssues,
			})
		}

		result = append(result, RenovateJobInfo{
			Name:             job.Name,
			CronExpression:   job.Spec.Schedule,
			NextSchedule:     s.scheduler.GetNextRunOnSchedule(job.Spec.Schedule),
			Projects:         projects,
			Platform:         platform,
			PlatformEndpoint: endpoint,
			ExecutionOptions: job.Status.ExecutionOptions,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

type jobActionRequest struct {
	RenovateJob string `json:"renovateJob"`
	Project     string `json:"project"`
}

func (s *Server) runRenovateForProject(w http.ResponseWriter, r *http.Request) {
	var params jobActionRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if params.RenovateJob == "" || params.Project == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing required parameters"})
		return
	}

	err := s.store.UpdateProjectStatus(
		r.Context(),
		params.Project,
		statestore.RenovateJobIdentifier{Name: params.RenovateJob},
		&statestore.RenovateStatusUpdate{
			Status: api.JobStatusScheduled,
		},
	)
	if err != nil {
		s.logger.Error("failed to schedule project", "project", params.Project, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to schedule project"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "renovate job triggered"})
}

func (s *Server) runRenovateForAllProjects(w http.ResponseWriter, r *http.Request) {
	var params struct {
		RenovateJob string `json:"renovateJob"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if params.RenovateJob == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing renovateJob"})
		return
	}

	err := s.store.UpdateProjectStatusBatched(
		r.Context(),
		func(p api.ProjectStatus) bool {
			return p.Status != api.JobStatusRunning && p.Status != api.JobStatusScheduled
		},
		statestore.RenovateJobIdentifier{Name: params.RenovateJob},
		&statestore.RenovateStatusUpdate{
			Status: api.JobStatusScheduled,
		},
	)
	if err != nil {
		s.logger.Error("failed to trigger all projects", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to trigger all projects"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "all projects triggered"})
}

func (s *Server) cancelRenovateForProject(w http.ResponseWriter, r *http.Request) {
	var params jobActionRequest
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if params.RenovateJob == "" || params.Project == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing required parameters"})
		return
	}

	err := s.store.CancelProjectJob(
		r.Context(),
		params.Project,
		statestore.RenovateJobIdentifier{Name: params.RenovateJob},
	)
	if err != nil {
		s.logger.Error("failed to cancel project", "project", params.Project, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to cancel project"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "project cancelled"})
}

func (s *Server) getRenovateJobLogs(w http.ResponseWriter, r *http.Request) {
	jobName := r.URL.Query().Get("renovate")
	project := r.URL.Query().Get("project")

	if jobName == "" || project == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing renovate or project parameter"})
		return
	}

	stream, err := s.store.StreamLogsForProject(
		r.Context(),
		statestore.RenovateJobIdentifier{Name: jobName},
		project,
	)
	if err != nil {
		s.logger.Error("failed to get logs", "project", project, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get logs"})
		return
	}
	defer func() { _ = stream.Close() }()

	// Stream as Server-Sent Events
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, _ := w.(http.Flusher)

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !json.Valid([]byte(line)) {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		s.logger.Error("error reading log stream", "project", project, "error", err)
		_, _ = fmt.Fprintf(w, "event: error\ndata: {\"error\":\"stream read error\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	_, _ = fmt.Fprint(w, "event: done\ndata: {}\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) runDiscovery(w http.ResponseWriter, r *http.Request) {
	var params struct {
		RenovateJob string `json:"renovateJob"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if params.RenovateJob == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing renovateJob"})
		return
	}

	job, err := s.store.GetRenovateJob(r.Context(), params.RenovateJob)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}

	if _, err := s.discovery.RunDiscovery(r.Context(), job); err != nil {
		s.logger.Error("failed to run discovery", "job", params.RenovateJob, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "discovery failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "discovery started"})
}

func (s *Server) updateExecutionOptions(w http.ResponseWriter, r *http.Request) {
	var params struct {
		RenovateJob string `json:"renovateJob"`
		Debug       bool   `json:"debug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if params.RenovateJob == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing renovateJob"})
		return
	}

	err := s.store.UpdateExecutionOptions(
		r.Context(),
		statestore.RenovateJobIdentifier{Name: params.RenovateJob},
		&api.RenovateExecutionOptions{Debug: params.Debug},
	)
	if err != nil {
		s.logger.Error("failed to update execution options", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "execution options updated"})
}

// --- UI ---

func (s *Server) registerUIRoutes(router *mux.Router) {
	// Check if static dir exists; if not, skip UI routes
	if _, err := os.Stat("./static"); os.IsNotExist(err) {
		s.logger.Info("no ./static directory found, UI routes disabled")
		return
	}

	fileServer := http.FileServer(http.Dir("./static/"))
	router.PathPrefix("/").Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve index.html for root and SPA routes
		path := r.URL.Path
		if path == "/" || path == "/index.html" {
			s.serveHTML(w, "./static/index.html")
			return
		}
		if path == "/logs" || path == "/logs.html" {
			s.serveHTML(w, "./static/pages/logs.html")
			return
		}
		// Try to serve static file
		fileServer.ServeHTTP(w, r)
	}))
}

func (s *Server) serveHTML(w http.ResponseWriter, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
