// Package statestore defines the RenovateJobManager interface and supporting types.
// This is the standalone (non-K8s) adaptation of the upstream crdManager interface.
package statestore

import (
	"context"
	"errors"
	"io"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
)

// RenovateJobManager is the interface for managing RenovateJob state.
// It provides methods to list, get, and update RenovateJobs and their associated projects.
type RenovateJobManager interface {
	// ListRenovateJobs lists all RenovateJobs.
	ListRenovateJobs(ctx context.Context) ([]RenovateJobIdentifier, error)
	// ListRenovateJobsFull lists all RenovateJobs with full object data.
	ListRenovateJobsFull(ctx context.Context) ([]api.RenovateJob, error)
	// GetRenovateJob retrieves a specific RenovateJob by name.
	GetRenovateJob(ctx context.Context, name string) (*api.RenovateJob, error)
	// GetProjectsForRenovateJob retrieves all projects associated with a specific RenovateJob.
	GetProjectsForRenovateJob(ctx context.Context, job RenovateJobIdentifier) ([]RenovateProjectStatus, error)
	// UpdateProjectStatus updates the status of a specific project within a RenovateJob.
	UpdateProjectStatus(ctx context.Context, project string, job RenovateJobIdentifier, status *RenovateStatusUpdate) error
	// UpdateProjectStatusBatched updates the status of multiple projects within a RenovateJob based on a filter function.
	UpdateProjectStatusBatched(ctx context.Context, fn func(p api.ProjectStatus) bool, job RenovateJobIdentifier, status *RenovateStatusUpdate) error
	// GetProjectsByStatus retrieves all projects with a specific status within a RenovateJob.
	GetProjectsByStatus(ctx context.Context, job RenovateJobIdentifier, status api.RenovateProjectStatus) ([]RenovateProjectStatus, error)
	// ReconcileProjects reconciles the list of projects in a RenovateJob
	// with the provided list. It returns the names of the projects that were
	// removed (present before, absent now).
	ReconcileProjects(ctx context.Context, job *api.RenovateJob, projects []string) ([]string, error)
	// SyncWebhooks ensures the operator's webhook exists on every project of
	// the RenovateJob and removes it from the given removed projects.
	SyncWebhooks(ctx context.Context, job RenovateJobIdentifier, removedProjects []string) error
	// CleanupWebhooks removes the operator's webhook from every project of the RenovateJob.
	CleanupWebhooks(ctx context.Context, job RenovateJobIdentifier) error
	// StreamLogsForProject returns an io.ReadCloser that streams NDJSON log lines for the given project.
	StreamLogsForProject(ctx context.Context, job RenovateJobIdentifier, project string) (io.ReadCloser, error)
	// StoreProjectLogs stores the raw log data for a project run.
	StoreProjectLogs(ctx context.Context, job RenovateJobIdentifier, project string, logData []byte) error
	// IsWebhookTokenValid checks if the provided token is valid for the webhook of the specified RenovateJob.
	IsWebhookTokenValid(ctx context.Context, job RenovateJobIdentifier, token string) (bool, error)
	// IsWebhookSignatureValid checks if the provided signature is valid for the webhook of the specified RenovateJob.
	IsWebhookSignatureValid(ctx context.Context, job RenovateJobIdentifier, signature string, body []byte) (bool, error)
	// IsWebhookStandardSignatureValid checks a Standard Webhooks signature against the configured signing keys.
	IsWebhookStandardSignatureValid(ctx context.Context, job RenovateJobIdentifier, msgID, timestamp, signature string, body []byte) (bool, error)
	// UpdateExecutionOptions updates the execution options for the specified RenovateJob.
	UpdateExecutionOptions(ctx context.Context, job RenovateJobIdentifier, options *api.RenovateExecutionOptions) error
	// UpdateWebhookEnabled toggles the webhook_enabled flag for the specified RenovateJob.
	UpdateWebhookEnabled(ctx context.Context, job RenovateJobIdentifier, enabled bool) error
	// CancelProjectJob stops the running Docker container for the given project and
	// transitions its status to cancelled, freeing the slot for the next dispatch.
	CancelProjectJob(ctx context.Context, project string, job RenovateJobIdentifier) error
}

// ErrProjectNotFound is returned when a project does not exist in a RenovateJob.
var ErrProjectNotFound = errors.New("project not found")

// ErrNotFound is returned when a RenovateJob does not exist.
var ErrNotFound = errors.New("not found")

// RenovateJobIdentifier uniquely identifies a RenovateJob.
type RenovateJobIdentifier struct {
	Name string
}

// Fullname returns the unique name for this identifier.
func (id *RenovateJobIdentifier) Fullname() string {
	return id.Name
}

// RenovateProjectStatus is a helper struct with denormalized project information.
type RenovateProjectStatus struct {
	Name                 string                    `json:"name"`
	Status               api.RenovateProjectStatus `json:"status"`
	LastRun              time.Time                 `json:"lastRun"`
	Priority             int32                     `json:"priority,omitempty"`
	RenovateResultStatus *string                   `json:"renovateResultStatus,omitempty"`
	Duration             *string                   `json:"duration,omitempty"`
	PRActivity           *api.PRActivity           `json:"prActivity,omitempty"`
	LogIssues            *api.LogIssues            `json:"logIssues,omitempty"`
}

// RenovateStatusUpdate carries the fields to update on a project status.
type RenovateStatusUpdate struct {
	Status               api.RenovateProjectStatus
	Duration             *string
	RenovateResultStatus *string
	PRActivity           *api.PRActivity
	LogIssues            *api.LogIssues
}
