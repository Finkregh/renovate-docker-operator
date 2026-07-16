// Package statestore — SQLite-backed implementation of RenovateJobManager.
// Stores all job and project state in a local SQLite database.
package statestore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oluf-tech/renovate-docker-operator/internal/api"
)

// standardWebhookTimestampTolerance bounds how far a webhook-timestamp may drift
// from the current time before the request is rejected as a potential replay.
const standardWebhookTimestampTolerance = 5 * time.Minute

// SQLiteStore implements RenovateJobManager using SQLite for persistence.
type SQLiteStore struct {
	writeDB       *sql.DB
	readDB        *sql.DB
	logger        *slog.Logger
	webhookTokens []string
}

// New creates a new SQLiteStore with the given database file path.
// It opens writer and reader connections, runs migrations, seeds a default job
// from environment variables if no jobs exist, and caches webhook tokens.
func New(dbPath string, logger *slog.Logger) (*SQLiteStore, error) {
	if logger == nil {
		logger = slog.Default()
	}

	writeDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON")
	if err != nil {
		return nil, fmt.Errorf("open write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)

	readDB, err := sql.Open("sqlite", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON")
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("open read db: %w", err)
	}

	if err := runMigrations(writeDB); err != nil {
		_ = readDB.Close()
		_ = writeDB.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	// Load webhook tokens from environment.
	var tokens []string
	if secret := os.Getenv("WEBHOOK_SECRET"); secret != "" {
		for _, t := range strings.Split(secret, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tokens = append(tokens, t)
			}
		}
	}

	s := &SQLiteStore{
		writeDB:       writeDB,
		readDB:        readDB,
		logger:        logger,
		webhookTokens: tokens,
	}

	// Seed default job if no jobs exist.
	if err := s.seedDefaultJob(); err != nil {
		_ = readDB.Close()
		_ = writeDB.Close()
		return nil, fmt.Errorf("seed default job: %w", err)
	}

	return s, nil
}

// Close closes both database connections.
func (s *SQLiteStore) Close() error {
	err1 := s.readDB.Close()
	err2 := s.writeDB.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// seedDefaultJob inserts a "default" RenovateJob if the jobs table is empty.
func (s *SQLiteStore) seedDefaultJob() error {
	ctx := context.Background()
	var count int
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM renovate_jobs`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	platform := envOrDefault("PLATFORM", "forgejo")
	endpoint := os.Getenv("PLATFORM_ENDPOINT")
	image := envOrDefault("RENOVATE_IMAGE", "renovate/renovate:latest")
	schedule := envOrDefault("CRON_SCHEDULE", "0 */4 * * *")
	parallelism := envOrDefaultInt("GLOBAL_PARALLELISM_LIMIT", 2)
	discoveryFilters := commaSepToJSON(os.Getenv("RENOVATE_DISCOVERY_FILTERS"))
	discoverTopics := commaSepToJSON(os.Getenv("RENOVATE_DISCOVER_TOPICS"))
	skipForks := boolToInt(os.Getenv("AUTODISCOVER_SKIP_FORKS"))

	_, err := s.writeDB.ExecContext(ctx, `INSERT INTO renovate_jobs
		(name, schedule, image, platform, endpoint, discovery_filters, discover_topics, skip_forks, parallelism)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"default", schedule, image, platform, endpoint, discoveryFilters, discoverTopics, skipForks, parallelism,
	)
	if err != nil {
		return fmt.Errorf("inserting default job: %w", err)
	}

	s.logger.Info("seeded default renovate job",
		"platform", platform,
		"endpoint", endpoint,
		"image", image,
	)
	return nil
}

// ---------------------------------------------------------------------------
// Interface implementation
// ---------------------------------------------------------------------------

// ListRenovateJobs returns all job identifiers.
func (s *SQLiteStore) ListRenovateJobs(ctx context.Context) ([]RenovateJobIdentifier, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT name FROM renovate_jobs`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var result []RenovateJobIdentifier
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result = append(result, RenovateJobIdentifier{Name: name})
	}
	return result, rows.Err()
}

// ListRenovateJobsFull returns all jobs with full details and projects.
func (s *SQLiteStore) ListRenovateJobsFull(ctx context.Context) ([]api.RenovateJob, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT name, schedule, image, platform, endpoint,
		discovery_filters, discover_topics, skip_forks, skip_pending_deletion,
		secret_ref, extra_env, parallelism, webhook_enabled, webhook_auth_enabled,
		webhook_sync_enabled, allowed_groups, debug_mode
		FROM renovate_jobs`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var jobs []api.RenovateJob
	for rows.Next() {
		job, err := scanRenovateJob(rows)
		if err != nil {
			return nil, err
		}
		// Attach projects
		projects, err := s.loadProjects(ctx, job.Name)
		if err != nil {
			return nil, err
		}
		job.Status.Projects = projects
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

// GetRenovateJob returns a single job by name.
func (s *SQLiteStore) GetRenovateJob(ctx context.Context, name string) (*api.RenovateJob, error) {
	row := s.readDB.QueryRowContext(ctx, `SELECT name, schedule, image, platform, endpoint,
		discovery_filters, discover_topics, skip_forks, skip_pending_deletion,
		secret_ref, extra_env, parallelism, webhook_enabled, webhook_auth_enabled,
		webhook_sync_enabled, allowed_groups, debug_mode
		FROM renovate_jobs WHERE name = ?`, name)

	job, err := scanRenovateJobRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	projects, err := s.loadProjects(ctx, name)
	if err != nil {
		return nil, err
	}
	job.Status.Projects = projects
	return job, nil
}

// GetProjectsForRenovateJob returns all project statuses for a job.
func (s *SQLiteStore) GetProjectsForRenovateJob(ctx context.Context, job RenovateJobIdentifier) ([]RenovateProjectStatus, error) {
	return s.loadRenovateProjectStatuses(ctx, job.Name)
}

// UpdateProjectStatus updates the status of a specific project.
func (s *SQLiteStore) UpdateProjectStatus(ctx context.Context, project string, job RenovateJobIdentifier, status *RenovateStatusUpdate) error {
	now := time.Now().UTC().Format(time.RFC3339)

	prActivityJSON, err := nullableJSON(status.PRActivity)
	if err != nil {
		return err
	}
	logIssuesJSON, err := nullableJSON(status.LogIssues)
	if err != nil {
		return err
	}

	res, err := s.writeDB.ExecContext(ctx, `UPDATE projects SET
		status = ?,
		last_run = ?,
		duration = ?,
		renovate_result_status = ?,
		pr_activity = ?,
		log_issues = ?
		WHERE job_name = ? AND full_name = ?`,
		string(status.Status),
		now,
		status.Duration,
		status.RenovateResultStatus,
		prActivityJSON,
		logIssuesJSON,
		job.Name,
		project,
	)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// UpdateProjectStatusBatched updates status for all matching projects in a batch.
func (s *SQLiteStore) UpdateProjectStatusBatched(ctx context.Context, fn func(p api.ProjectStatus) bool, job RenovateJobIdentifier, status *RenovateStatusUpdate) error {
	// Load all projects for the job.
	projects, err := s.loadProjects(ctx, job.Name)
	if err != nil {
		return err
	}

	// Filter and collect matching project names.
	var matching []string
	for _, p := range projects {
		if fn(p) {
			matching = append(matching, p.Name)
		}
	}
	if len(matching) == 0 {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	prActivityJSON, err := nullableJSON(status.PRActivity)
	if err != nil {
		return err
	}
	logIssuesJSON, err := nullableJSON(status.LogIssues)
	if err != nil {
		return err
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `UPDATE projects SET
		status = ?,
		last_run = ?,
		duration = ?,
		renovate_result_status = ?,
		pr_activity = ?,
		log_issues = ?
		WHERE job_name = ? AND full_name = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, name := range matching {
		if _, err := stmt.ExecContext(ctx,
			string(status.Status),
			now,
			status.Duration,
			status.RenovateResultStatus,
			prActivityJSON,
			logIssuesJSON,
			job.Name,
			name,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetProjectsByStatus returns projects matching a given status.
func (s *SQLiteStore) GetProjectsByStatus(ctx context.Context, job RenovateJobIdentifier, status api.RenovateProjectStatus) ([]RenovateProjectStatus, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT full_name, status, priority, last_run, duration,
		renovate_result_status, pr_activity, log_issues
		FROM projects WHERE job_name = ? AND status = ?`, job.Name, string(status))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanProjectStatuses(rows)
}

// ReconcileProjects adds new projects and removes stale ones, returning removed names.
func (s *SQLiteStore) ReconcileProjects(ctx context.Context, job *api.RenovateJob, projects []string) ([]string, error) {
	// Get existing project names.
	rows, err := s.readDB.QueryContext(ctx, `SELECT full_name FROM projects WHERE job_name = ?`, job.Name)
	if err != nil {
		return nil, err
	}
	existingSet := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		existingSet[name] = struct{}{}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Build new set for lookup.
	newSet := make(map[string]struct{}, len(projects))
	for _, p := range projects {
		newSet[p] = struct{}{}
	}

	// Find removed projects.
	var removed []string
	for name := range existingSet {
		if _, ok := newSet[name]; !ok {
			removed = append(removed, name)
		}
	}

	// Find new projects to insert.
	var toInsert []string
	for _, p := range projects {
		if _, ok := existingSet[p]; !ok {
			toInsert = append(toInsert, p)
		}
	}

	// Apply changes in a transaction.
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Remove projects no longer present.
	if len(removed) > 0 {
		for _, name := range removed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE job_name = ? AND full_name = ?`, job.Name, name); err != nil {
				return nil, err
			}
		}
	}

	// Insert new projects.
	if len(toInsert) > 0 {
		now := time.Now().UTC().Format(time.RFC3339)
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO projects (job_name, full_name, status, last_run) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return nil, err
		}
		defer func() { _ = stmt.Close() }()
		for _, name := range toInsert {
			if _, err := stmt.ExecContext(ctx, job.Name, name, string(api.JobStatusScheduled), now); err != nil {
				return nil, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return removed, nil
}

// SyncWebhooks synchronizes webhooks for removed projects (not yet implemented).
func (s *SQLiteStore) SyncWebhooks(_ context.Context, job RenovateJobIdentifier, removedProjects []string) error {
	s.logger.Info("SyncWebhooks called (not implemented in state store)", "job", job.Name, "removed", len(removedProjects))
	return nil
}

// CleanupWebhooks removes all webhooks for a job (not yet implemented).
func (s *SQLiteStore) CleanupWebhooks(_ context.Context, job RenovateJobIdentifier) error {
	s.logger.Info("CleanupWebhooks called (not implemented in state store)", "job", job.Name)
	return nil
}

// StreamLogsForProject returns a reader for project logs.
func (s *SQLiteStore) StreamLogsForProject(ctx context.Context, job RenovateJobIdentifier, project string) (io.ReadCloser, error) {
	var logData []byte
	err := s.readDB.QueryRowContext(ctx,
		`SELECT log_data FROM job_logs WHERE job_name = ? AND project_name = ?`,
		job.Name, project,
	).Scan(&logData)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(logData)), nil
}

// IsWebhookTokenValid checks if the given token matches any stored webhook token.
func (s *SQLiteStore) IsWebhookTokenValid(_ context.Context, _ RenovateJobIdentifier, token string) (bool, error) {
	for _, t := range s.webhookTokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			return true, nil
		}
	}
	return false, nil
}

// IsWebhookSignatureValid validates an HMAC-SHA256 webhook signature.
func (s *SQLiteStore) IsWebhookSignatureValid(_ context.Context, _ RenovateJobIdentifier, signature string, body []byte) (bool, error) {
	for _, token := range s.webhookTokens {
		expected := computeHMAC256(body, token)
		if hmac.Equal([]byte(signature), []byte(expected)) {
			return true, nil
		}
	}
	return false, nil
}

// IsWebhookStandardSignatureValid validates a Standard Webhooks signature.
func (s *SQLiteStore) IsWebhookStandardSignatureValid(_ context.Context, _ RenovateJobIdentifier, msgID, timestamp, signature string, body []byte) (bool, error) {
	if msgID == "" || signature == "" {
		return false, nil
	}

	if !isStandardWebhookTimestampFresh(timestamp, time.Now()) {
		s.logger.Info("rejecting webhook: signature timestamp outside tolerance", "timestamp", timestamp)
		return false, nil
	}

	signedContent := msgID + "." + timestamp + "." + string(body)
	for _, token := range s.webhookTokens {
		key, ok := decodeStandardWebhookSigningKey(token)
		if !ok {
			continue
		}
		expected := computeStandardWebhookSignature(key, signedContent)
		if matchesAnyStandardWebhookSignature(signature, expected) {
			return true, nil
		}
	}
	return false, nil
}

// UpdateExecutionOptions updates execution options (e.g., debug mode) for a job.
func (s *SQLiteStore) UpdateExecutionOptions(ctx context.Context, job RenovateJobIdentifier, options *api.RenovateExecutionOptions) error {
	debugMode := 0
	if options != nil && options.Debug {
		debugMode = 1
	}
	_, err := s.writeDB.ExecContext(ctx, `UPDATE renovate_jobs SET debug_mode = ? WHERE name = ?`, debugMode, job.Name)
	return err
}

// CancelProjectJob cancels a running project job.
func (s *SQLiteStore) CancelProjectJob(ctx context.Context, project string, job RenovateJobIdentifier) error {
	// Get the container ID before updating status.
	var containerID sql.NullString
	_ = s.readDB.QueryRowContext(ctx,
		`SELECT current_container_id FROM projects WHERE job_name = ? AND full_name = ? AND status = 'running'`,
		job.Name, project,
	).Scan(&containerID)

	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE projects SET status = ?, current_container_id = NULL WHERE job_name = ? AND full_name = ? AND status = 'running'`,
		string(api.JobStatusCancelled), job.Name, project,
	)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return ErrProjectNotFound
	}

	if containerID.Valid && containerID.String != "" {
		s.logger.Info("project cancelled, container should be stopped", "project", project, "containerID", containerID.String)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func scanRenovateJob(rows *sql.Rows) (*api.RenovateJob, error) {
	var (
		name, schedule, image, platform, endpoint string
		discoveryFiltersJSON, discoverTopicsJSON  string
		skipForks, skipPendingDeletion            int
		secretRef                                 string
		extraEnvJSON                              string
		parallelism                               int32
		webhookEnabled, webhookAuth, webhookSync  int
		allowedGroupsJSON                         string
		debugMode                                 int
	)

	if err := rows.Scan(&name, &schedule, &image, &platform, &endpoint,
		&discoveryFiltersJSON, &discoverTopicsJSON, &skipForks, &skipPendingDeletion,
		&secretRef, &extraEnvJSON, &parallelism, &webhookEnabled, &webhookAuth,
		&webhookSync, &allowedGroupsJSON, &debugMode,
	); err != nil {
		return nil, err
	}

	return buildRenovateJob(name, schedule, image, platform, endpoint,
		discoveryFiltersJSON, discoverTopicsJSON, skipForks, skipPendingDeletion,
		secretRef, extraEnvJSON, parallelism, webhookEnabled, webhookAuth,
		webhookSync, allowedGroupsJSON, debugMode)
}

func scanRenovateJobRow(row *sql.Row) (*api.RenovateJob, error) {
	var (
		name, schedule, image, platform, endpoint string
		discoveryFiltersJSON, discoverTopicsJSON  string
		skipForks, skipPendingDeletion            int
		secretRef                                 string
		extraEnvJSON                              string
		parallelism                               int32
		webhookEnabled, webhookAuth, webhookSync  int
		allowedGroupsJSON                         string
		debugMode                                 int
	)

	if err := row.Scan(&name, &schedule, &image, &platform, &endpoint,
		&discoveryFiltersJSON, &discoverTopicsJSON, &skipForks, &skipPendingDeletion,
		&secretRef, &extraEnvJSON, &parallelism, &webhookEnabled, &webhookAuth,
		&webhookSync, &allowedGroupsJSON, &debugMode,
	); err != nil {
		return nil, err
	}

	return buildRenovateJob(name, schedule, image, platform, endpoint,
		discoveryFiltersJSON, discoverTopicsJSON, skipForks, skipPendingDeletion,
		secretRef, extraEnvJSON, parallelism, webhookEnabled, webhookAuth,
		webhookSync, allowedGroupsJSON, debugMode)
}

func buildRenovateJob(
	name, schedule, image, platform, endpoint string,
	discoveryFiltersJSON, discoverTopicsJSON string,
	skipForks, skipPendingDeletion int,
	secretRef string,
	extraEnvJSON string,
	parallelism int32,
	webhookEnabled, webhookAuth, webhookSync int,
	allowedGroupsJSON string,
	debugMode int,
) (*api.RenovateJob, error) {
	var discoveryFilters []string
	if err := json.Unmarshal([]byte(discoveryFiltersJSON), &discoveryFilters); err != nil {
		discoveryFilters = nil
	}

	var discoverTopics []string
	if err := json.Unmarshal([]byte(discoverTopicsJSON), &discoverTopics); err != nil {
		discoverTopics = nil
	}

	var extraEnv []api.EnvVar
	if err := json.Unmarshal([]byte(extraEnvJSON), &extraEnv); err != nil {
		extraEnv = nil
	}

	var allowedGroups []string
	if err := json.Unmarshal([]byte(allowedGroupsJSON), &allowedGroups); err != nil {
		allowedGroups = nil
	}

	job := &api.RenovateJob{
		Name: name,
		Spec: api.RenovateJobSpec{
			Schedule: schedule,
			Image:    image,
			Provider: &api.RenovateProvider{
				Name:     platform,
				Endpoint: endpoint,
			},
			DiscoveryFilters:    discoveryFilters,
			DiscoverTopics:      discoverTopics,
			SkipForks:           skipForks != 0,
			SkipPendingDeletion: skipPendingDeletion != 0,
			SecretRef:           secretRef,
			ExtraEnv:            extraEnv,
			Parallelism:         parallelism,
			AllowedGroups:       allowedGroups,
		},
	}

	if webhookEnabled != 0 {
		job.Spec.Webhook = &api.RenovateWebhook{
			Enabled: true,
			Authentication: &api.RenovateWebhookAuth{
				Enabled: webhookAuth != 0,
			},
			Sync: &api.RenovateWebhookSync{
				Enabled: webhookSync != 0,
			},
		}
	}

	if debugMode != 0 {
		job.Status.ExecutionOptions = &api.RenovateExecutionOptions{Debug: true}
	}

	return job, nil
}

func (s *SQLiteStore) loadProjects(ctx context.Context, jobName string) ([]api.ProjectStatus, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT full_name, status, priority, last_run, duration,
		renovate_result_status, pr_activity, log_issues
		FROM projects WHERE job_name = ?`, jobName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var projects []api.ProjectStatus
	for rows.Next() {
		var (
			name, status          string
			priority              int32
			lastRunStr            sql.NullString
			duration              sql.NullString
			renovateResultStatus  sql.NullString
			prActivityJSON        sql.NullString
			logIssuesJSON         sql.NullString
		)
		if err := rows.Scan(&name, &status, &priority, &lastRunStr, &duration,
			&renovateResultStatus, &prActivityJSON, &logIssuesJSON); err != nil {
			return nil, err
		}

		p := api.ProjectStatus{
			Name:     name,
			Status:   api.RenovateProjectStatus(status),
			Priority: priority,
		}

		if lastRunStr.Valid {
			if t, err := time.Parse(time.RFC3339, lastRunStr.String); err == nil {
				p.LastRun = t
			}
		}
		if duration.Valid {
			p.Duration = &duration.String
		}
		if renovateResultStatus.Valid {
			p.RenovateResultStatus = &renovateResultStatus.String
		}
		if prActivityJSON.Valid {
			var pa api.PRActivity
			if err := json.Unmarshal([]byte(prActivityJSON.String), &pa); err == nil {
				p.PRActivity = &pa
			}
		}
		if logIssuesJSON.Valid {
			var li api.LogIssues
			if err := json.Unmarshal([]byte(logIssuesJSON.String), &li); err == nil {
				p.LogIssues = &li
			}
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *SQLiteStore) loadRenovateProjectStatuses(ctx context.Context, jobName string) ([]RenovateProjectStatus, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT full_name, status, priority, last_run, duration,
		renovate_result_status, pr_activity, log_issues
		FROM projects WHERE job_name = ?`, jobName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanProjectStatuses(rows)
}

func scanProjectStatuses(rows *sql.Rows) ([]RenovateProjectStatus, error) {
	var result []RenovateProjectStatus
	for rows.Next() {
		var (
			name, status         string
			priority             int32
			lastRunStr           sql.NullString
			duration             sql.NullString
			renovateResultStatus sql.NullString
			prActivityJSON       sql.NullString
			logIssuesJSON        sql.NullString
		)
		if err := rows.Scan(&name, &status, &priority, &lastRunStr, &duration,
			&renovateResultStatus, &prActivityJSON, &logIssuesJSON); err != nil {
			return nil, err
		}

		p := RenovateProjectStatus{
			Name:     name,
			Status:   api.RenovateProjectStatus(status),
			Priority: priority,
		}

		if lastRunStr.Valid {
			if t, err := time.Parse(time.RFC3339, lastRunStr.String); err == nil {
				p.LastRun = t
			}
		}
		if duration.Valid {
			p.Duration = &duration.String
		}
		if renovateResultStatus.Valid {
			p.RenovateResultStatus = &renovateResultStatus.String
		}
		if prActivityJSON.Valid {
			var pa api.PRActivity
			if err := json.Unmarshal([]byte(prActivityJSON.String), &pa); err == nil {
				p.PRActivity = &pa
			}
		}
		if logIssuesJSON.Valid {
			var li api.LogIssues
			if err := json.Unmarshal([]byte(logIssuesJSON.String), &li); err == nil {
				p.LogIssues = &li
			}
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Crypto helpers (ported from upstream)
// ---------------------------------------------------------------------------

func computeHMAC256(message []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(message)
	return "sha256=" + fmt.Sprintf("%x", mac.Sum(nil))
}

func decodeStandardWebhookSigningKey(secret string) ([]byte, bool) {
	if secret == "" {
		return nil, false
	}
	if rest, found := strings.CutPrefix(secret, "whsec_"); found {
		decoded, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return nil, false
		}
		return decoded, true
	}
	if decoded, err := base64.StdEncoding.DecodeString(secret); err == nil {
		return decoded, true
	}
	return []byte(secret), true
}

func computeStandardWebhookSignature(key []byte, signedContent string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signedContent))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func matchesAnyStandardWebhookSignature(header, expected string) bool {
	for _, part := range strings.Fields(header) {
		version, sig, found := strings.Cut(part, ",")
		if !found || version != "v1" {
			continue
		}
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return true
		}
	}
	return false
}

func isStandardWebhookTimestampFresh(timestamp string, now time.Time) bool {
	secs, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return false
	}
	delta := now.Sub(time.Unix(secs, 0))
	if delta < 0 {
		delta = -delta
	}
	return delta <= standardWebhookTimestampTolerance
}

// ---------------------------------------------------------------------------
// Utility helpers
// ---------------------------------------------------------------------------

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrDefaultInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func boolToInt(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "true" || s == "1" || s == "yes" {
		return 1
	}
	return 0
}

func commaSepToJSON(s string) string {
	if s == "" {
		return "[]"
	}
	var items []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return "[]"
	}
	data, _ := json.Marshal(items)
	return string(data)
}

func nullableJSON(v any) (sql.NullString, error) {
	if v == nil {
		return sql.NullString{}, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(data), Valid: true}, nil
}
