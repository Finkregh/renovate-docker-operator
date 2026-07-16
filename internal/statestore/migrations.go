package statestore

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations is an ordered list of SQL DDL strings.
// The schema_version table tracks which have been applied.
var migrations = []string{
	migration0001,
}

const migration0001 = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS renovate_jobs (
    name TEXT PRIMARY KEY,
    schedule TEXT NOT NULL DEFAULT '0 */4 * * *',
    image TEXT NOT NULL DEFAULT 'renovate/renovate:latest',
    platform TEXT NOT NULL DEFAULT 'forgejo',
    endpoint TEXT NOT NULL DEFAULT '',
    discovery_filters TEXT NOT NULL DEFAULT '[]',
    discover_topics TEXT NOT NULL DEFAULT '[]',
    skip_forks INTEGER NOT NULL DEFAULT 0,
    skip_pending_deletion INTEGER NOT NULL DEFAULT 0,
    secret_ref TEXT NOT NULL DEFAULT '',
    extra_env TEXT NOT NULL DEFAULT '[]',
    parallelism INTEGER NOT NULL DEFAULT 2,
    webhook_enabled INTEGER NOT NULL DEFAULT 0,
    webhook_auth_enabled INTEGER NOT NULL DEFAULT 0,
    webhook_sync_enabled INTEGER NOT NULL DEFAULT 0,
    allowed_groups TEXT NOT NULL DEFAULT '[]',
    debug_mode INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS projects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_name TEXT NOT NULL REFERENCES renovate_jobs(name) ON DELETE CASCADE,
    full_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'scheduled',
    priority INTEGER NOT NULL DEFAULT 0,
    last_run TEXT,
    duration TEXT,
    renovate_result_status TEXT,
    pr_activity TEXT,
    log_issues TEXT,
    current_container_id TEXT,
    generation INTEGER NOT NULL DEFAULT 0,
    UNIQUE(job_name, full_name)
);

CREATE TABLE IF NOT EXISTS webhook_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_name TEXT NOT NULL REFERENCES renovate_jobs(name) ON DELETE CASCADE,
    token TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS execution_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_name TEXT NOT NULL,
    project_name TEXT NOT NULL,
    container_id TEXT,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    exit_code INTEGER,
    status TEXT NOT NULL DEFAULT 'running',
    log_snippet TEXT
);

CREATE TABLE IF NOT EXISTS job_logs (
    job_name TEXT NOT NULL,
    project_name TEXT NOT NULL,
    log_data BLOB,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (job_name, project_name)
);

CREATE INDEX IF NOT EXISTS idx_projects_job_status ON projects(job_name, status);
CREATE INDEX IF NOT EXISTS idx_projects_job_name ON projects(job_name, full_name);
CREATE INDEX IF NOT EXISTS idx_execution_history_project ON execution_history(job_name, project_name);
`

// runMigrations applies any pending schema migrations to the database.
func runMigrations(db *sql.DB) error {
	ctx := context.Background()

	// Ensure the schema_migrations table exists (bootstrap).
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	if err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}

	// Determine the current schema version.
	var current int
	row := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("reading current schema version: %w", err)
	}

	// Apply migrations that haven't been applied yet.
	for i := current; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}

		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("applying migration %d: %w", i+1, err)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, i+1); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recording migration %d: %w", i+1, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}

	return nil
}
