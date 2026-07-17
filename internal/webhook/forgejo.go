// Package webhook implements Forgejo webhook handling for renovate-docker-operator.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/oluf-tech/renovate-docker-operator/internal/api"
	"github.com/oluf-tech/renovate-docker-operator/internal/statestore"
)

// ErrNoMatchingJob is returned when no RenovateJob matches the webhook request.
var ErrNoMatchingJob = errors.New("no matching renovate job found")

// ErrAuthenticationFailed is returned when webhook signature validation fails.
var ErrAuthenticationFailed = errors.New("authentication failed")

// ForgejoEvent represents the payload structure of a Forgejo webhook.
type ForgejoEvent struct {
	Action      string              `json:"action"`
	PullRequest *ForgejoPullRequest `json:"pull_request,omitempty"`
	Issue       *ForgejoIssue       `json:"issue,omitempty"`
	Repository  ForgejoRepository   `json:"repository"`
	Ref         string              `json:"ref"`
	Before      string              `json:"before"`
	After       string              `json:"after"`
}

// ForgejoPullRequest contains pull request data from a Forgejo webhook.
type ForgejoPullRequest struct {
	ID     int          `json:"id"`
	Merged bool         `json:"merged"`
	Number int          `json:"number"`
	Body   string       `json:"body"`
	User   *ForgejoUser `json:"user,omitempty"`
}

// ForgejoIssue contains issue data from a Forgejo webhook.
type ForgejoIssue struct {
	ID     int    `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// ForgejoRepository contains repository data from a Forgejo webhook.
type ForgejoRepository struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
}

// ForgejoUser contains user data from a Forgejo webhook.
type ForgejoUser struct {
	Login string `json:"login"`
}

// Handler processes Forgejo webhook requests and schedules Renovate jobs.
type Handler struct {
	store  statestore.RenovateJobManager
	logger *slog.Logger
}

// NewHandler creates a new webhook handler.
func NewHandler(store statestore.RenovateJobManager, logger *slog.Logger) *Handler {
	return &Handler{
		store:  store,
		logger: logger,
	}
}

// HandleForgejo processes a Forgejo webhook POST request.
func (h *Handler) HandleForgejo(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-Forgejo-Event")
	if event == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing X-Forgejo-Event header"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read request body"})
		return
	}

	var payload ForgejoEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger.Error("failed to decode Forgejo webhook payload", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to decode payload"})
		return
	}

	valid, reason := isValidForgejoEvent(event, &payload)
	if !valid {
		h.logger.Info("ignoring Forgejo webhook event",
			"event", event,
			"repository", payload.Repository.FullName,
			"reason", reason,
		)
		writeJSON(w, http.StatusOK, map[string]string{"message": "event ignored", "reason": reason})
		return
	}

	// Resolve the job from query params
	jobName := r.URL.Query().Get("job")
	project := payload.Repository.FullName

	jobID, err := h.findAndAuthenticateJob(r.Context(), jobName, project, r, body)
	if err != nil {
		h.handleResolverError(w, err)
		return
	}

	h.logger.Info("received Forgejo event",
		"event", event,
		"repository", project,
		"action", payload.Action,
	)

	err = h.store.UpdateProjectStatus(
		r.Context(),
		project,
		jobID,
		&statestore.RenovateStatusUpdate{
			Status: api.JobStatusScheduled,
		},
	)
	if err != nil {
		if errors.Is(err, statestore.ErrProjectNotFound) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project not found: " + project})
		} else {
			h.logger.Error("failed to schedule project", "project", project, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to process webhook"})
		}
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"message":    "renovate job scheduled",
		"repository": project,
	})
}

// HandleSchedule processes manual scheduling via POST /webhook/v1/schedule?project=X&job=Y.
func (h *Handler) HandleSchedule(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing project query parameter"})
		return
	}

	jobName := r.URL.Query().Get("job")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read request body"})
		return
	}

	jobID, err := h.findAndAuthenticateJob(r.Context(), jobName, project, r, body)
	if err != nil {
		h.handleResolverError(w, err)
		return
	}

	err = h.store.UpdateProjectStatus(
		r.Context(),
		project,
		jobID,
		&statestore.RenovateStatusUpdate{
			Status: api.JobStatusScheduled,
		},
	)
	if err != nil {
		if errors.Is(err, statestore.ErrProjectNotFound) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project not found: " + project})
		} else {
			h.logger.Error("failed to schedule project", "project", project, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to process webhook"})
		}
		return
	}

	w.WriteHeader(http.StatusOK)
	h.logger.Info("manually scheduled project via webhook", "project", project, "job", jobID.Name)
}

// findAndAuthenticateJob resolves the RenovateJob for the request and validates authentication.
func (h *Handler) findAndAuthenticateJob(ctx context.Context, jobName, project string, r *http.Request, body []byte) (statestore.RenovateJobIdentifier, error) {
	jobs, err := h.store.ListRenovateJobsFull(ctx)
	if err != nil {
		return statestore.RenovateJobIdentifier{}, err
	}

	candidates := filterCandidates(jobs, jobName, project)
	if len(candidates) == 0 {
		return statestore.RenovateJobIdentifier{}, ErrNoMatchingJob
	}

	for _, job := range candidates {
		id := statestore.RenovateJobIdentifier{Name: job.Name}

		// No auth required if webhook authentication is disabled
		if job.Spec.Webhook == nil || job.Spec.Webhook.Authentication == nil || !job.Spec.Webhook.Authentication.Enabled {
			return id, nil
		}

		// Try to authenticate
		if h.authenticate(ctx, id, r, body) {
			return id, nil
		}
	}

	return statestore.RenovateJobIdentifier{}, ErrAuthenticationFailed
}

// authenticate validates the webhook request using available credentials.
// It tries ALL signature methods and returns true if ANY succeeds.
func (h *Handler) authenticate(ctx context.Context, jobID statestore.RenovateJobIdentifier, r *http.Request, body []byte) bool {
	// Try X-Forgejo-Signature (raw hex HMAC-SHA256)
	if sig := r.Header.Get("X-Forgejo-Signature"); sig != "" {
		if ok, err := h.store.IsWebhookSignatureValid(ctx, jobID, sig, body); err == nil && ok {
			return true
		}
	}

	// Try X-Gitea-Signature (raw hex HMAC-SHA256)
	if sig := r.Header.Get("X-Gitea-Signature"); sig != "" {
		if ok, err := h.store.IsWebhookSignatureValid(ctx, jobID, sig, body); err == nil && ok {
			return true
		}
	}

	// Try X-Hub-Signature-256 (sha256=<hex> format — strip prefix before comparison)
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		raw := strings.TrimPrefix(sig, "sha256=")
		if ok, err := h.store.IsWebhookSignatureValid(ctx, jobID, raw, body); err == nil && ok {
			return true
		}
	}

	// Try Authorization header / Bearer token
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		token := authHeader
		if strings.HasPrefix(authHeader, "Bearer ") {
			parts := strings.SplitN(authHeader, " ", 2)
			token = strings.TrimSpace(parts[1])
		}
		if ok, err := h.store.IsWebhookTokenValid(ctx, jobID, token); err == nil && ok {
			return true
		}
	}

	// Try X-Gitlab-Token
	if token := r.Header.Get("X-Gitlab-Token"); token != "" {
		if ok, err := h.store.IsWebhookTokenValid(ctx, jobID, token); err == nil && ok {
			return true
		}
	}

	return false
}

// filterCandidates filters jobs based on job name and project membership.
func filterCandidates(jobs []api.RenovateJob, jobName, project string) []api.RenovateJob {
	out := make([]api.RenovateJob, 0, len(jobs))
	for _, job := range jobs {
		if jobName != "" && job.Name != jobName {
			continue
		}
		if job.Spec.Webhook == nil || !job.Spec.Webhook.Enabled {
			continue
		}
		if !hasProject(job.Status.Projects, project) {
			continue
		}
		out = append(out, job)
	}
	return out
}

func hasProject(projects []api.ProjectStatus, project string) bool {
	for _, p := range projects {
		if p.Name == project {
			return true
		}
	}
	return false
}

func (h *Handler) handleResolverError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNoMatchingJob) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if errors.Is(err, ErrAuthenticationFailed) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	h.logger.Error("unexpected error resolving webhook job", "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
}

// isValidForgejoEvent validates whether a Forgejo webhook event should trigger Renovate.
func isValidForgejoEvent(event string, payload *ForgejoEvent) (bool, string) {
	switch event {
	case "issues":
		if payload.Action != "edited" {
			return false, "issue action is not edited"
		}
		if payload.Issue == nil || payload.Issue.Body == "" {
			return false, "no issue body"
		}
		if !verifyRenovateDescriptionChange(payload.Issue.Body) {
			return false, "not a valid renovate checkbox change"
		}
		return true, ""

	case "pull_request":
		if payload.PullRequest == nil {
			return false, "no pull request in payload"
		}
		if payload.PullRequest.Merged {
			return true, ""
		}
		if payload.PullRequest.Body == "" {
			return false, "no pull request body"
		}
		if !isRenovateContent(payload.PullRequest.Body) {
			return false, "not a Renovate pull request"
		}
		switch payload.Action {
		case "closed", "reopened":
			return true, ""
		case "edited":
			if !hasCheckboxBeenChecked(payload.PullRequest.Body) {
				return false, "no checked checkbox"
			}
			return true, ""
		default:
			return false, "pull request action is not edited, closed, or reopened"
		}

	case "push":
		if payload.Ref == "" {
			return false, "no ref in push payload"
		}
		if !strings.HasPrefix(payload.Ref, "refs/heads/") {
			return false, "not a branch push"
		}
		if payload.After == "0000000000000000000000000000000000000000" {
			return false, "branch deletion"
		}
		return true, ""

	default:
		return false, "unsupported event type: " + event
	}
}

// isRenovateContent checks if the description is from Renovate.
func isRenovateContent(description string) bool {
	if description == "" {
		return false
	}

	patterns := []string{
		"## Detected Dependencies",
		"<!-- rebase-check -->",
		"<!--renovate-debug:",
		"<!-- rebase-all-open-prs -->",
		"<!-- rebase-branch=",
		"<!-- approve-all-pending-prs -->",
		"<!-- approvePr-branch=",
		"<!-- approve-branch=",
		"<!-- recreate-branch=",
		"<!-- unschedule-branch=",
		"<!-- create-config-migration-pr -->",
		"<!-- create-all-awaiting-schedule-prs -->",
		"<!-- create-all-rate-limited-prs -->",
		"<!-- unlimit-branch=",
		"<!-- manual job -->",
	}

	for _, pattern := range patterns {
		if strings.Contains(description, pattern) {
			return true
		}
	}
	return false
}

// hasCheckboxBeenChecked checks if there's a checked Renovate checkbox.
func hasCheckboxBeenChecked(body string) bool {
	return strings.Contains(body, "- [x]") || strings.Contains(body, "- [X]")
}

// verifyRenovateDescriptionChange verifies Renovate content with a checked checkbox.
func verifyRenovateDescriptionChange(body string) bool {
	return isRenovateContent(body) && hasCheckboxBeenChecked(body)
}


func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
