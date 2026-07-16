// Package api contains standalone type definitions for the renovate-docker-operator.
// Derived from mogenius/renovate-operator api/v1alpha1 with all Kubernetes dependencies removed.
package api

import "time"

// EnvVar is a simple name/value environment variable (replaces corev1.EnvVar).
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// RenovateJobSpec defines the desired state of a RenovateJob.
type RenovateJobSpec struct {
	// Cron schedule in standard cron format
	Schedule string `json:"schedule"`
	// Renovate Docker image to use
	Image string `json:"image"`
	// Renovate Provider Information to fill "RENOVATE_ENDPOINT" and "RENOVATE_PLATFORM" environment variables
	Provider *RenovateProvider `json:"provider"`
	// Filter to select which projects to process, will be concatenated using , separator
	DiscoveryFilters []string `json:"discoveryFilters,omitempty"`
	// Topics to discover projects from, will be concatenated using , separator
	DiscoverTopics []string `json:"discoverTopics,omitempty"`
	// If true, forked repositories discovered during autodiscovery will be excluded
	SkipForks bool `json:"skipForks,omitempty"`
	// If true, repositories marked for delayed deletion will be excluded (GitLab only)
	SkipPendingDeletion bool `json:"skipPendingDeletion,omitempty"`
	// Reference to the secret containing the renovate config
	SecretRef string `json:"secretRef,omitempty"`
	// Additional environment variables to set in the renovate container
	ExtraEnv []EnvVar `json:"extraEnv,omitempty"`
	// Maximum number of projects to process in parallel
	Parallelism int32 `json:"parallelism"`
	// Configuration for webhooks to trigger renovate runs
	Webhook *RenovateWebhook `json:"webhook,omitempty"`
	// Groups allowed to view this RenovateJob when authentication is enabled.
	AllowedGroups []string `json:"allowedGroups,omitempty"`
}

// RenovateProvider holds platform information.
type RenovateProvider struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint,omitempty"`
}

// RenovateWebhook configures webhooks that can trigger renovate runs.
type RenovateWebhook struct {
	Enabled        bool                 `json:"enabled"`
	Authentication *RenovateWebhookAuth `json:"authentication,omitempty"`
	Sync           *RenovateWebhookSync `json:"sync,omitempty"`
}

// RenovateWebhookAuth holds authentication configuration for webhooks.
type RenovateWebhookAuth struct {
	Enabled   bool                        `json:"enabled"`
	SecretRef *RenovateSecretKeyReference `json:"secretRef,omitempty"`
}

// RenovateWebhookSync configures automatic webhook syncing onto discovered repositories.
type RenovateWebhookSync struct {
	// Flag to enable the automatic repo webhook sync
	Enabled bool `json:"enabled"`
	// Optional reference to a secret key holding the platform token used for webhook management.
	SecretRef *RenovateSecretKeyReference `json:"secretRef,omitempty"`
}

// RenovateSecretKeyReference is a reference to a secret name and key.
type RenovateSecretKeyReference struct {
	Name string `json:"name,omitempty"`
	Key  string `json:"key,omitempty"`
}

// RenovateExecutionOptions controls per-run execution behaviour.
type RenovateExecutionOptions struct {
	// If true, the renovate job will be executed with RENOVATE_LOG_LEVEL=debug
	Debug bool `json:"debug,omitempty"`
}

// RenovateJobStatus defines the observed state of a RenovateJob.
type RenovateJobStatus struct {
	Projects         []ProjectStatus           `json:"projects,omitempty"`
	ExecutionOptions *RenovateExecutionOptions `json:"executionOptions,omitempty"`
}

// ProjectStatus holds the status of a single project within a RenovateJob.
type ProjectStatus struct {
	Name                 string                `json:"name"`
	LastRun              time.Time             `json:"lastRun"`
	Duration             *string               `json:"duration,omitempty"`
	Status               RenovateProjectStatus `json:"status"`
	Priority             int32                 `json:"priority,omitempty"`
	RenovateResultStatus *string               `json:"renovateResultStatus,omitempty"`
	PRActivity           *PRActivity           `json:"prActivity,omitempty"`
	LogIssues            *LogIssues            `json:"logIssues,omitempty"`
}

// RenovateProjectStatus represents the lifecycle state of a project run.
type RenovateProjectStatus string

// Possible values for RenovateProjectStatus.
const (
	JobStatusScheduled RenovateProjectStatus = "scheduled"
	JobStatusRunning   RenovateProjectStatus = "running"
	JobStatusCompleted RenovateProjectStatus = "completed"
	JobStatusFailed    RenovateProjectStatus = "failed"
	JobStatusCancelled RenovateProjectStatus = "cancelled"
)

// PRAction represents what happened to a PR in a Renovate run.
type PRAction string

// Possible values for PRAction.
const (
	PRActionAutomerged    PRAction = "automerged"
	PRActionCreated       PRAction = "created"
	PRActionUpdated       PRAction = "updated"
	PRActionNeedsApproval PRAction = "needs-approval"
	PRActionUnchanged     PRAction = "unchanged"
)

// PRDetail represents a single PR found in Renovate logs.
type PRDetail struct {
	Branch string   `json:"branch"`
	Number int      `json:"number,omitempty"`
	Title  string   `json:"title,omitempty"`
	Action PRAction `json:"action"`
}

// PRActivity contains aggregate counts and individual details of PR activity from a run.
type PRActivity struct {
	Automerged    int        `json:"automerged"`
	Created       int        `json:"created"`
	Updated       int        `json:"updated"`
	NeedsApproval int        `json:"needsApproval"`
	Unchanged     int        `json:"unchanged"`
	PRs           []PRDetail `json:"prs,omitempty"`
	Truncated     bool       `json:"truncated,omitempty"`
}

// LogIssue represents a single warning or error from Renovate logs.
type LogIssue struct {
	Level   int    `json:"level"`
	Message string `json:"message"`
}

// LogIssues contains aggregate counts and individual issue messages from a Renovate run.
type LogIssues struct {
	WarnCount  int        `json:"warnCount"`
	ErrorCount int        `json:"errorCount"`
	Issues     []LogIssue `json:"issues,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
}

// RenovateJob is the standalone representation of a renovate job (no K8s ObjectMeta/TypeMeta).
type RenovateJob struct {
	Name   string            `json:"name"`
	Spec   RenovateJobSpec   `json:"spec,omitempty"`
	Status RenovateJobStatus `json:"status,omitempty"`
}

// Fullname returns the unique identifier for this job.
func (j *RenovateJob) Fullname() string {
	return j.Name
}
