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

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/statestore"
)

// ErrNoMatchingJob is returned when no RenovateJob matches the webhook request.
var ErrNoMatchingJob = errors.New("no matching renovate job found")

// ErrWebhookDisabled is returned when the matched job has webhooks disabled.
var ErrWebhookDisabled = errors.New("webhooks are not enabled on the matched job")

// ErrProjectNotDiscovered is returned when the project has not been discovered yet.
var ErrProjectNotDiscovered = errors.New("project not found in job; run discovery first")

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
	store          statestore.RenovateJobManager
	logger         *slog.Logger
	maxRequestBody int64
}

// NewHandler creates a new webhook handler.
func NewHandler(store statestore.RenovateJobManager, logger *slog.Logger, maxRequestBody int64) *Handler {
	return &Handler{
		store:          store,
		logger:         logger,
		maxRequestBody: maxRequestBody,
	}
}

// HandleForgejo processes a Forgejo webhook POST request.
// HandleForgejo processes incoming Forgejo webhook events.
// NOTE: This handler schedules individual projects for Renovate runs —
// it does NOT trigger discovery. If a future "new repo created" webhook
// should trigger discovery, use discovery.Agent.RunDiscoveryAsync to avoid
// blocking the webhook response.
func (h *Handler) HandleForgejo(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-Forgejo-Event")
	if event == "" {
		h.logger.Warn("webhook rejected: missing X-Forgejo-Event header",
			"remote", r.RemoteAddr)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing X-Forgejo-Event header"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isMaxBytesError(err) {
			h.logger.Warn("webhook rejected: request body too large",
				"event", event, "remote", r.RemoteAddr)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		h.logger.Error("webhook rejected: failed to read request body",
			"event", event, "remote", r.RemoteAddr, "error", err)
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

	jobID, err := h.findAndAuthenticateJob(r.Context(), jobName, project, w, r, body)
	if err != nil {
		h.handleResolverError(w, err, event, project)
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
			h.logger.Warn("webhook rejected: project not found",
				"event", event, "project", project)
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
		h.logger.Warn("schedule rejected: missing project query parameter",
			"remote", r.RemoteAddr)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing project query parameter"})
		return
	}

	jobName := r.URL.Query().Get("job")

	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isMaxBytesError(err) {
			h.logger.Warn("schedule rejected: request body too large",
				"project", project, "remote", r.RemoteAddr)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		h.logger.Error("schedule rejected: failed to read request body",
			"project", project, "remote", r.RemoteAddr, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read request body"})
		return
	}

	jobID, err := h.findAndAuthenticateJob(r.Context(), jobName, project, w, r, body)
	if err != nil {
		h.handleResolverError(w, err, "schedule", project)
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
			h.logger.Warn("schedule rejected: project not found",
				"project", project)
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
func (h *Handler) findAndAuthenticateJob(ctx context.Context, jobName, project string, w http.ResponseWriter, r *http.Request, body []byte) (statestore.RenovateJobIdentifier, error) {
	jobs, err := h.store.ListRenovateJobsFull(ctx)
	if err != nil {
		return statestore.RenovateJobIdentifier{}, err
	}

	candidates, resolveErr := filterCandidates(jobs, jobName, project)
	if len(candidates) == 0 {
		return statestore.RenovateJobIdentifier{}, resolveErr
	}

	var lastAuthResult *AuthResult
	for _, job := range candidates {
		id := statestore.RenovateJobIdentifier{Name: job.Name}

		// No auth required if webhook authentication is disabled
		if job.Spec.Webhook == nil || job.Spec.Webhook.Authentication == nil || !job.Spec.Webhook.Authentication.Enabled {
			SetAuthOnResponse(w, &AuthResult{Required: false})
			return id, nil
		}

		// Try to authenticate
		ok, authResult := h.authenticate(ctx, id, r, body)
		lastAuthResult = authResult
		if ok {
			SetAuthOnResponse(w, authResult)
			return id, nil
		}
	}

	// Store the last auth result before returning failure
	if lastAuthResult != nil {
		SetAuthOnResponse(w, lastAuthResult)
	}

	return statestore.RenovateJobIdentifier{}, ErrAuthenticationFailed
}

// authenticate validates the webhook request using available credentials.
// It tries ALL signature methods and returns true if ANY succeeds.
// The returned AuthResult records which methods were attempted and their outcome.
func (h *Handler) authenticate(ctx context.Context, jobID statestore.RenovateJobIdentifier, r *http.Request, body []byte) (bool, *AuthResult) {
	result := &AuthResult{Required: true, Success: false}

	// Try X-Forgejo-Signature (raw hex HMAC-SHA256)
	if sig := r.Header.Get("X-Forgejo-Signature"); sig != "" {
		ok, err := h.store.IsWebhookSignatureValid(ctx, jobID, sig, body)
		verified := err == nil && ok
		result.Methods = append(result.Methods, AuthAttempt{Type: "X-Forgejo-Signature", Verified: verified})
		if verified {
			result.Success = true
			return true, result
		}
	}

	// Try X-Gitea-Signature (raw hex HMAC-SHA256)
	if sig := r.Header.Get("X-Gitea-Signature"); sig != "" {
		ok, err := h.store.IsWebhookSignatureValid(ctx, jobID, sig, body)
		verified := err == nil && ok
		result.Methods = append(result.Methods, AuthAttempt{Type: "X-Gitea-Signature", Verified: verified})
		if verified {
			result.Success = true
			return true, result
		}
	}

	// Try X-Hub-Signature-256 (sha256=<hex> format — strip prefix before comparison)
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		raw := strings.TrimPrefix(sig, "sha256=")
		ok, err := h.store.IsWebhookSignatureValid(ctx, jobID, raw, body)
		verified := err == nil && ok
		result.Methods = append(result.Methods, AuthAttempt{Type: "X-Hub-Signature-256", Verified: verified})
		if verified {
			result.Success = true
			return true, result
		}
	}

	// Try Authorization header / Bearer token
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		token := authHeader
		if strings.HasPrefix(authHeader, "Bearer ") {
			parts := strings.SplitN(authHeader, " ", 2)
			token = strings.TrimSpace(parts[1])
		}
		ok, err := h.store.IsWebhookTokenValid(ctx, jobID, token)
		verified := err == nil && ok
		result.Methods = append(result.Methods, AuthAttempt{Type: "Authorization", Verified: verified})
		if verified {
			result.Success = true
			return true, result
		}
	}

	// Try X-Gitlab-Token
	if token := r.Header.Get("X-Gitlab-Token"); token != "" {
		ok, err := h.store.IsWebhookTokenValid(ctx, jobID, token)
		verified := err == nil && ok
		result.Methods = append(result.Methods, AuthAttempt{Type: "X-Gitlab-Token", Verified: verified})
		if verified {
			result.Success = true
			return true, result
		}
	}

	return false, result
}

// filterCandidates filters jobs based on job name, webhook enabled state, and project membership.
// Returns the matching candidates and, when empty, a specific error explaining why no job matched.
func filterCandidates(jobs []api.RenovateJob, jobName, project string) ([]api.RenovateJob, error) {
	out := make([]api.RenovateJob, 0, len(jobs))
	var (
		nameMatched     bool
		webhookDisabled bool
		projectMissing  bool
	)
	for _, job := range jobs {
		if jobName != "" && job.Name != jobName {
			continue
		}
		nameMatched = true
		if job.Spec.Webhook == nil || !job.Spec.Webhook.Enabled {
			webhookDisabled = true
			continue
		}
		if !hasProject(job.Status.Projects, project) {
			projectMissing = true
			continue
		}
		out = append(out, job)
	}

	if len(out) > 0 {
		return out, nil
	}

	// Return the most specific reason for no match.
	if !nameMatched {
		return nil, ErrNoMatchingJob
	}
	if webhookDisabled {
		return nil, ErrWebhookDisabled
	}
	if projectMissing {
		return nil, ErrProjectNotDiscovered
	}
	return nil, ErrNoMatchingJob
}

func hasProject(projects []api.ProjectStatus, project string) bool {
	for _, p := range projects {
		if p.Name == project {
			return true
		}
	}
	return false
}

func (h *Handler) handleResolverError(w http.ResponseWriter, err error, event, repository string) {
	switch {
	case errors.Is(err, ErrNoMatchingJob):
		h.logger.Warn("webhook rejected: no matching job",
			"event", event, "repository", repository)
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no matching job found for this webhook request",
		})
	case errors.Is(err, ErrWebhookDisabled):
		h.logger.Warn("webhook rejected: webhooks disabled on matched job",
			"event", event, "repository", repository)
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "webhooks are not enabled on the matched job; set webhook_enabled=1 in the database or enable via the UI",
		})
	case errors.Is(err, ErrProjectNotDiscovered):
		h.logger.Warn("webhook rejected: project not discovered",
			"event", event, "repository", repository)
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "project not found in job; ensure discovery has run and the project is registered",
		})
	case errors.Is(err, ErrAuthenticationFailed):
		h.logger.Warn("webhook rejected: authentication failed",
			"event", event, "repository", repository)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	default:
		h.logger.Error("unexpected error resolving webhook job",
			"event", event, "repository", repository, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
	}
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

// isMaxBytesError checks if the error is due to exceeding the max request body size.
func isMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}
