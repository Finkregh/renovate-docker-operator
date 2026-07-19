package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/statestore"
)

func TestIsValidForgejoEvent_Issues(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		payload *ForgejoEvent
		valid   bool
		reason  string
	}{
		{
			name:  "valid issue edited with renovate checkbox",
			event: "issues",
			payload: &ForgejoEvent{
				Action: "edited",
				Issue: &ForgejoIssue{
					Body: "## Detected Dependencies\n- [x] some checkbox",
				},
			},
			valid:  true,
			reason: "",
		},
		{
			name:  "issue action is not edited",
			event: "issues",
			payload: &ForgejoEvent{
				Action: "opened",
				Issue: &ForgejoIssue{
					Body: "## Detected Dependencies\n- [x] check",
				},
			},
			valid:  false,
			reason: "issue action is not edited",
		},
		{
			name:  "no issue body",
			event: "issues",
			payload: &ForgejoEvent{
				Action: "edited",
				Issue:  &ForgejoIssue{},
			},
			valid:  false,
			reason: "no issue body",
		},
		{
			name:  "not renovate content",
			event: "issues",
			payload: &ForgejoEvent{
				Action: "edited",
				Issue: &ForgejoIssue{
					Body: "just a normal issue body",
				},
			},
			valid:  false,
			reason: "not a valid renovate checkbox change",
		},
		{
			name:  "renovate content but no checked checkbox",
			event: "issues",
			payload: &ForgejoEvent{
				Action: "edited",
				Issue: &ForgejoIssue{
					Body: "## Detected Dependencies\n- [ ] unchecked",
				},
			},
			valid:  false,
			reason: "not a valid renovate checkbox change",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid, reason := isValidForgejoEvent(tt.event, tt.payload)
			if valid != tt.valid {
				t.Errorf("expected valid=%v, got %v", tt.valid, valid)
			}
			if reason != tt.reason {
				t.Errorf("expected reason=%q, got %q", tt.reason, reason)
			}
		})
	}
}

func TestIsValidForgejoEvent_PullRequest(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		payload *ForgejoEvent
		valid   bool
		reason  string
	}{
		{
			name:  "merged PR is always valid",
			event: "pull_request",
			payload: &ForgejoEvent{
				Action: "closed",
				PullRequest: &ForgejoPullRequest{
					Merged: true,
					Body:   "something",
				},
			},
			valid:  true,
			reason: "",
		},
		{
			name:  "closed renovate PR",
			event: "pull_request",
			payload: &ForgejoEvent{
				Action: "closed",
				PullRequest: &ForgejoPullRequest{
					Body: "<!-- rebase-check -->",
				},
			},
			valid:  true,
			reason: "",
		},
		{
			name:  "reopened renovate PR",
			event: "pull_request",
			payload: &ForgejoEvent{
				Action: "reopened",
				PullRequest: &ForgejoPullRequest{
					Body: "<!-- rebase-check -->",
				},
			},
			valid:  true,
			reason: "",
		},
		{
			name:  "edited PR with checkbox",
			event: "pull_request",
			payload: &ForgejoEvent{
				Action: "edited",
				PullRequest: &ForgejoPullRequest{
					Body: "<!-- rebase-check -->\n- [x] rebase",
				},
			},
			valid:  true,
			reason: "",
		},
		{
			name:  "edited PR without checkbox",
			event: "pull_request",
			payload: &ForgejoEvent{
				Action: "edited",
				PullRequest: &ForgejoPullRequest{
					Body: "<!-- rebase-check -->\n- [ ] unchecked",
				},
			},
			valid:  false,
			reason: "no checked checkbox",
		},
		{
			name:  "non-renovate PR",
			event: "pull_request",
			payload: &ForgejoEvent{
				Action: "closed",
				PullRequest: &ForgejoPullRequest{
					Body: "just a normal PR",
				},
			},
			valid:  false,
			reason: "not a Renovate pull request",
		},
		{
			name:  "unsupported action",
			event: "pull_request",
			payload: &ForgejoEvent{
				Action: "opened",
				PullRequest: &ForgejoPullRequest{
					Body: "<!-- rebase-check -->",
				},
			},
			valid:  false,
			reason: "pull request action is not edited, closed, or reopened",
		},
		{
			name:    "no pull request in payload",
			event:   "pull_request",
			payload: &ForgejoEvent{Action: "closed"},
			valid:   false,
			reason:  "no pull request in payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid, reason := isValidForgejoEvent(tt.event, tt.payload)
			if valid != tt.valid {
				t.Errorf("expected valid=%v, got %v", tt.valid, valid)
			}
			if reason != tt.reason {
				t.Errorf("expected reason=%q, got %q", tt.reason, reason)
			}
		})
	}
}

func TestIsValidForgejoEvent_Push(t *testing.T) {
	tests := []struct {
		name    string
		payload *ForgejoEvent
		valid   bool
		reason  string
	}{
		{
			name: "valid push to main",
			payload: &ForgejoEvent{
				Ref:   "refs/heads/main",
				After: "abc123def456abc123def456abc123def456abc1",
			},
			valid:  true,
			reason: "",
		},
		{
			name: "valid push to feature branch",
			payload: &ForgejoEvent{
				Ref:   "refs/heads/feat/foo",
				After: "abc123def456abc123def456abc123def456abc1",
			},
			valid:  true,
			reason: "",
		},
		{
			name: "branch creation",
			payload: &ForgejoEvent{
				Ref:    "refs/heads/new",
				Before: "0000000000000000000000000000000000000000",
				After:  "abc123def456abc123def456abc123def456abc1",
			},
			valid:  true,
			reason: "",
		},
		{
			name: "tag push rejected",
			payload: &ForgejoEvent{
				Ref:   "refs/tags/v1.0",
				After: "abc123def456abc123def456abc123def456abc1",
			},
			valid:  false,
			reason: "not a branch push",
		},
		{
			name: "branch deletion rejected",
			payload: &ForgejoEvent{
				Ref:   "refs/heads/main",
				After: "0000000000000000000000000000000000000000",
			},
			valid:  false,
			reason: "branch deletion",
		},
		{
			name: "empty ref rejected",
			payload: &ForgejoEvent{
				Ref: "",
			},
			valid:  false,
			reason: "no ref in push payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid, reason := isValidForgejoEvent("push", tt.payload)
			if valid != tt.valid {
				t.Errorf("expected valid=%v, got %v", tt.valid, valid)
			}
			if reason != tt.reason {
				t.Errorf("expected reason=%q, got %q", tt.reason, reason)
			}
		})
	}
}

func TestIsValidForgejoEvent_UnsupportedEvent(t *testing.T) {
	valid, reason := isValidForgejoEvent("create", &ForgejoEvent{})
	if valid {
		t.Error("expected create event to be invalid")
	}
	if reason != "unsupported event type: create" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestIsRenovateContent(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected bool
	}{
		{"empty", "", false},
		{"dependency dashboard", "## Detected Dependencies\nstuff", true},
		{"rebase check", "some text\n<!-- rebase-check -->\nmore", true},
		{"renovate debug", "<!--renovate-debug: info-->", true},
		{"rebase branch", "<!-- rebase-branch=main -->", true},
		{"normal content", "just a regular description", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRenovateContent(tt.body); got != tt.expected {
				t.Errorf("isRenovateContent(%q) = %v, want %v", tt.body, got, tt.expected)
			}
		})
	}
}

func TestHasCheckboxBeenChecked(t *testing.T) {
	tests := []struct {
		body     string
		expected bool
	}{
		{"- [x] checked", true},
		{"- [X] checked", true},
		{"- [ ] unchecked", false},
		{"", false},
		{"no checkboxes here", false},
	}

	for _, tt := range tests {
		if got := hasCheckboxBeenChecked(tt.body); got != tt.expected {
			t.Errorf("hasCheckboxBeenChecked(%q) = %v, want %v", tt.body, got, tt.expected)
		}
	}
}

// ---------------------------------------------------------------------------
// Mock store for authenticate tests
// ---------------------------------------------------------------------------

// mockStore implements a minimal statestore.RenovateJobManager for testing authenticate().
type mockStore struct {
	// secret is the HMAC signing key (raw hex comparison)
	secret string
	// token is the valid bearer/gitlab token
	token string
}

func (m *mockStore) IsWebhookSignatureValid(_ context.Context, _ statestore.RenovateJobIdentifier, signature string, body []byte) (bool, error) {
	mac := hmac.New(sha256.New, []byte(m.secret))
	mac.Write(body)
	expected := fmt.Sprintf("%x", mac.Sum(nil))
	if hmac.Equal([]byte(signature), []byte(expected)) {
		return true, nil
	}
	return false, nil
}

func (m *mockStore) IsWebhookTokenValid(_ context.Context, _ statestore.RenovateJobIdentifier, token string) (bool, error) {
	if m.token != "" && token == m.token {
		return true, nil
	}
	return false, nil
}

// Unused interface methods — stubs to satisfy the interface.
func (m *mockStore) ListRenovateJobs(context.Context) ([]statestore.RenovateJobIdentifier, error) {
	return nil, nil
}
func (m *mockStore) ListRenovateJobsFull(context.Context) ([]api.RenovateJob, error) {
	return nil, nil
}
func (m *mockStore) GetRenovateJob(context.Context, string) (*api.RenovateJob, error) {
	return nil, nil
}
func (m *mockStore) GetProjectsForRenovateJob(context.Context, statestore.RenovateJobIdentifier) ([]statestore.RenovateProjectStatus, error) {
	return nil, nil
}
func (m *mockStore) UpdateProjectStatus(context.Context, string, statestore.RenovateJobIdentifier, *statestore.RenovateStatusUpdate) error {
	return nil
}
func (m *mockStore) UpdateProjectStatusBatched(context.Context, func(api.ProjectStatus) bool, statestore.RenovateJobIdentifier, *statestore.RenovateStatusUpdate) error {
	return nil
}
func (m *mockStore) GetProjectsByStatus(context.Context, statestore.RenovateJobIdentifier, api.RenovateProjectStatus) ([]statestore.RenovateProjectStatus, error) {
	return nil, nil
}
func (m *mockStore) ReconcileProjects(context.Context, *api.RenovateJob, []string) ([]string, error) {
	return nil, nil
}
func (m *mockStore) SyncWebhooks(context.Context, statestore.RenovateJobIdentifier, []string) error {
	return nil
}
func (m *mockStore) CleanupWebhooks(context.Context, statestore.RenovateJobIdentifier) error {
	return nil
}
func (m *mockStore) StreamLogsForProject(context.Context, statestore.RenovateJobIdentifier, string) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockStore) StoreProjectLogs(context.Context, statestore.RenovateJobIdentifier, string, []byte) error {
	return nil
}
func (m *mockStore) IsWebhookStandardSignatureValid(context.Context, statestore.RenovateJobIdentifier, string, string, string, []byte) (bool, error) {
	return false, nil
}
func (m *mockStore) UpdateExecutionOptions(context.Context, statestore.RenovateJobIdentifier, *api.RenovateExecutionOptions) error {
	return nil
}
func (m *mockStore) UpdateWebhookEnabled(context.Context, statestore.RenovateJobIdentifier, bool) error {
	return nil
}
func (m *mockStore) CancelProjectJob(context.Context, string, statestore.RenovateJobIdentifier) error {
	return nil
}
func (m *mockStore) SetDiscoveryStatus(context.Context, string, string, *time.Time, *time.Time, string) error {
	return nil
}
func (m *mockStore) GetDiscoveryStatus(context.Context, string) (*statestore.DiscoveryStatus, error) {
	return nil, nil
}
func (m *mockStore) ResetOrphanedDiscoveryStatus(context.Context) error {
	return nil
}

// ---------------------------------------------------------------------------
// Tests for authenticate()
// ---------------------------------------------------------------------------

func TestAuthenticate(t *testing.T) {
	const secret = "my-webhook-secret"
	const token = "my-bearer-token"
	body := []byte(`{"action":"edited"}`)

	// Compute the valid raw hex HMAC for this body+secret.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validHex := fmt.Sprintf("%x", mac.Sum(nil))

	handler := NewHandler(&mockStore{secret: secret, token: token}, slog.Default(), 2*1024*1024)
	jobID := statestore.RenovateJobIdentifier{Name: "test-job"}

	tests := []struct {
		name           string
		headers        map[string]string
		wantOK         bool
		wantSuccess    bool
		wantMethodsLen int
	}{
		{
			name:           "X-Forgejo-Signature valid raw hex",
			headers:        map[string]string{"X-Forgejo-Signature": validHex},
			wantOK:         true,
			wantSuccess:    true,
			wantMethodsLen: 1,
		},
		{
			name:           "X-Forgejo-Signature invalid hex",
			headers:        map[string]string{"X-Forgejo-Signature": "deadbeef"},
			wantOK:         false,
			wantSuccess:    false,
			wantMethodsLen: 1,
		},
		{
			name:           "X-Hub-Signature-256 valid with sha256= prefix",
			headers:        map[string]string{"X-Hub-Signature-256": "sha256=" + validHex},
			wantOK:         true,
			wantSuccess:    true,
			wantMethodsLen: 1,
		},
		{
			name:           "X-Hub-Signature-256 invalid",
			headers:        map[string]string{"X-Hub-Signature-256": "sha256=badhex"},
			wantOK:         false,
			wantSuccess:    false,
			wantMethodsLen: 1,
		},
		{
			name:           "X-Gitea-Signature valid raw hex",
			headers:        map[string]string{"X-Gitea-Signature": validHex},
			wantOK:         true,
			wantSuccess:    true,
			wantMethodsLen: 1,
		},
		{
			name:           "Authorization Bearer token valid",
			headers:        map[string]string{"Authorization": "Bearer " + token},
			wantOK:         true,
			wantSuccess:    true,
			wantMethodsLen: 1,
		},
		{
			name:           "Authorization Bearer token invalid",
			headers:        map[string]string{"Authorization": "Bearer wrong-token"},
			wantOK:         false,
			wantSuccess:    false,
			wantMethodsLen: 1,
		},
		{
			name:           "X-Gitlab-Token valid",
			headers:        map[string]string{"X-Gitlab-Token": token},
			wantOK:         true,
			wantSuccess:    true,
			wantMethodsLen: 1,
		},
		{
			name:           "X-Gitlab-Token invalid",
			headers:        map[string]string{"X-Gitlab-Token": "wrong-token"},
			wantOK:         false,
			wantSuccess:    false,
			wantMethodsLen: 1,
		},
		{
			name:           "no auth headers",
			headers:        map[string]string{},
			wantOK:         false,
			wantSuccess:    false,
			wantMethodsLen: 0,
		},
		{
			name: "multiple headers present - Forgejo sends all - succeeds via X-Forgejo-Signature",
			headers: map[string]string{
				"X-Forgejo-Signature": validHex,
				"X-Gitea-Signature":   validHex,
				"X-Hub-Signature-256": "sha256=" + validHex,
			},
			wantOK:         true,
			wantSuccess:    true,
			wantMethodsLen: 1, // short-circuits on first success
		},
		{
			name: "multiple headers - first invalid but second valid",
			headers: map[string]string{
				"X-Forgejo-Signature": "invalid",
				"X-Gitea-Signature":   validHex,
			},
			wantOK:         true,
			wantSuccess:    true,
			wantMethodsLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhook", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			ok, authResult := handler.authenticate(context.Background(), jobID, req, body)
			if ok != tt.wantOK {
				t.Errorf("authenticate() = %v, want %v", ok, tt.wantOK)
			}
			if authResult == nil {
				t.Fatal("expected non-nil AuthResult")
			}
			if authResult.Required != true {
				t.Errorf("AuthResult.Required = %v, want true", authResult.Required)
			}
			if authResult.Success != tt.wantSuccess {
				t.Errorf("AuthResult.Success = %v, want %v", authResult.Success, tt.wantSuccess)
			}
			if len(authResult.Methods) != tt.wantMethodsLen {
				t.Errorf("len(AuthResult.Methods) = %d, want %d", len(authResult.Methods), tt.wantMethodsLen)
			}
		})
	}
}
