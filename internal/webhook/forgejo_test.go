package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
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

func TestIsValidForgejoEvent_UnsupportedEvent(t *testing.T) {
	valid, reason := isValidForgejoEvent("push", &ForgejoEvent{})
	if valid {
		t.Error("expected push event to be invalid")
	}
	if reason != "unsupported event type: push" {
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

func TestValidateHMACSHA256(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"action":"edited"}`)

	// Empty signature should not be valid
	if ValidateHMACSHA256(secret, body, "") {
		t.Error("empty signature should not be valid")
	}

	// Compute correct signature and verify
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !ValidateHMACSHA256(secret, body, expected) {
		t.Error("valid signature should pass")
	}

	// Wrong signature should fail
	if ValidateHMACSHA256(secret, body, "deadbeef") {
		t.Error("wrong signature should not pass")
	}
}
