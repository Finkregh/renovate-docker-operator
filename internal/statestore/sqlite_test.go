package statestore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	// Set required env for seeding.
	t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
	t.Setenv("ROP_WEBHOOK_SECRET", "secret1,secret2")

	store, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestNew_CreatesDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
	t.Setenv("ROP_WEBHOOK_SECRET", "")

	store, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	// DB file should exist.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("DB file not created: %v", err)
	}

	// Default job should be seeded.
	ctx := context.Background()
	jobs, err := store.ListRenovateJobs(ctx)
	if err != nil {
		t.Fatalf("ListRenovateJobs failed: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Name != "default" {
		t.Fatalf("expected job name 'default', got %q", jobs[0].Name)
	}
}

func TestMigrations_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
	t.Setenv("ROP_WEBHOOK_SECRET", "")

	store1, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("first New() failed: %v", err)
	}
	_ = store1.Close()

	// Open again — migrations should be idempotent.
	store2, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("second New() failed: %v", err)
	}
	defer func() { _ = store2.Close() }()

	// Still one job (not double-seeded).
	ctx := context.Background()
	jobs, err := store2.ListRenovateJobs(ctx)
	if err != nil {
		t.Fatalf("ListRenovateJobs failed: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job after re-open, got %d", len(jobs))
	}
}

func TestReconcileProjects_InsertsAndRemoves(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	job := &api.RenovateJob{Name: "default"}

	// First reconcile — add projects.
	removed, err := store.ReconcileProjects(ctx, job, []string{"org/repo1", "org/repo2", "org/repo3"})
	if err != nil {
		t.Fatalf("ReconcileProjects failed: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("expected no removed projects, got %v", removed)
	}

	// Verify projects exist.
	projects, err := store.GetProjectsForRenovateJob(ctx, RenovateJobIdentifier{Name: "default"})
	if err != nil {
		t.Fatalf("GetProjectsForRenovateJob failed: %v", err)
	}
	if len(projects) != 3 {
		t.Fatalf("expected 3 projects, got %d", len(projects))
	}

	// Second reconcile — remove one, add one.
	removed, err = store.ReconcileProjects(ctx, job, []string{"org/repo1", "org/repo3", "org/repo4"})
	if err != nil {
		t.Fatalf("ReconcileProjects (2) failed: %v", err)
	}
	if len(removed) != 1 || removed[0] != "org/repo2" {
		t.Fatalf("expected removed=[org/repo2], got %v", removed)
	}

	// Verify final state.
	projects, err = store.GetProjectsForRenovateJob(ctx, RenovateJobIdentifier{Name: "default"})
	if err != nil {
		t.Fatalf("GetProjectsForRenovateJob (2) failed: %v", err)
	}
	if len(projects) != 3 {
		t.Fatalf("expected 3 projects after reconcile, got %d", len(projects))
	}
	names := make(map[string]bool)
	for _, p := range projects {
		names[p.Name] = true
	}
	for _, expected := range []string{"org/repo1", "org/repo3", "org/repo4"} {
		if !names[expected] {
			t.Errorf("expected project %q to exist", expected)
		}
	}
}

func TestUpdateProjectStatus(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	job := &api.RenovateJob{Name: "default"}
	_, _ = store.ReconcileProjects(ctx, job, []string{"org/repo1"})

	dur := "45s"
	err := store.UpdateProjectStatus(ctx, "org/repo1", RenovateJobIdentifier{Name: "default"}, &RenovateStatusUpdate{
		Status:   api.JobStatusRunning,
		Duration: &dur,
	})
	if err != nil {
		t.Fatalf("UpdateProjectStatus failed: %v", err)
	}

	// Verify.
	projects, _ := store.GetProjectsByStatus(ctx, RenovateJobIdentifier{Name: "default"}, api.JobStatusRunning)
	if len(projects) != 1 {
		t.Fatalf("expected 1 running project, got %d", len(projects))
	}
	if projects[0].Name != "org/repo1" {
		t.Fatalf("expected org/repo1, got %q", projects[0].Name)
	}
	if projects[0].Duration == nil || *projects[0].Duration != "45s" {
		t.Fatalf("expected duration=45s, got %v", projects[0].Duration)
	}

	// Update non-existent project should fail.
	err = store.UpdateProjectStatus(ctx, "org/nonexistent", RenovateJobIdentifier{Name: "default"}, &RenovateStatusUpdate{
		Status: api.JobStatusFailed,
	})
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("expected ErrProjectNotFound, got %v", err)
	}
}

func TestWebhookTokenValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	jobID := RenovateJobIdentifier{Name: "default"}

	// Valid token.
	valid, err := store.IsWebhookTokenValid(ctx, jobID, "secret1")
	if err != nil {
		t.Fatalf("IsWebhookTokenValid failed: %v", err)
	}
	if !valid {
		t.Fatal("expected token 'secret1' to be valid")
	}

	// Invalid token.
	valid, err = store.IsWebhookTokenValid(ctx, jobID, "wrong")
	if err != nil {
		t.Fatalf("IsWebhookTokenValid failed: %v", err)
	}
	if valid {
		t.Fatal("expected token 'wrong' to be invalid")
	}

	// Signature validation.
	body := []byte(`{"ref":"main"}`)
	mac := hmac.New(sha256.New, []byte("secret1"))
	mac.Write(body)
	sig := fmt.Sprintf("%x", mac.Sum(nil))

	valid, err = store.IsWebhookSignatureValid(ctx, jobID, sig, body)
	if err != nil {
		t.Fatalf("IsWebhookSignatureValid failed: %v", err)
	}
	if !valid {
		t.Fatal("expected signature to be valid")
	}

	// Invalid signature.
	valid, _ = store.IsWebhookSignatureValid(ctx, jobID, "bad", body)
	if valid {
		t.Fatal("expected bad signature to be invalid")
	}
}

func TestStandardWebhookSignature(t *testing.T) {
	// Generate a key and create a whsec_ token.
	rawKey := []byte("test-signing-key-32-bytes-long!!")
	token := "whsec_" + base64.StdEncoding.EncodeToString(rawKey)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
	t.Setenv("ROP_WEBHOOK_SECRET", token)

	store, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	jobID := RenovateJobIdentifier{Name: "default"}

	body := []byte(`{"action":"push"}`)
	msgID := "msg_123"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	signedContent := msgID + "." + ts + "." + string(body)

	mac := hmac.New(sha256.New, rawKey)
	mac.Write([]byte(signedContent))
	sig := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	valid, err := store.IsWebhookStandardSignatureValid(ctx, jobID, msgID, ts, sig, body)
	if err != nil {
		t.Fatalf("IsWebhookStandardSignatureValid failed: %v", err)
	}
	if !valid {
		t.Fatal("expected standard webhook signature to be valid")
	}

	// Expired timestamp should fail.
	oldTs := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	oldSignedContent := msgID + "." + oldTs + "." + string(body)
	mac2 := hmac.New(sha256.New, rawKey)
	mac2.Write([]byte(oldSignedContent))
	oldSig := "v1," + base64.StdEncoding.EncodeToString(mac2.Sum(nil))

	valid, err = store.IsWebhookStandardSignatureValid(ctx, jobID, msgID, oldTs, oldSig, body)
	if err != nil {
		t.Fatalf("IsWebhookStandardSignatureValid (old) failed: %v", err)
	}
	if valid {
		t.Fatal("expected expired timestamp to be rejected")
	}

	// Wrong signature should fail.
	valid, _ = store.IsWebhookStandardSignatureValid(ctx, jobID, msgID, ts, "v1,badsig", body)
	if valid {
		t.Fatal("expected wrong signature to be rejected")
	}
}

func TestMigration0002_DebugModeDefault(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Open raw DB and apply only migration0001 manually
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}

	_, err = db.ExecContext(context.Background(), migration0001)
	if err != nil {
		t.Fatalf("migration0001 failed: %v", err)
	}
	_, err = db.ExecContext(context.Background(), `INSERT INTO schema_migrations (version) VALUES (1)`)
	if err != nil {
		t.Fatalf("insert schema_migrations failed: %v", err)
	}

	// Insert a job with debug_mode=0 (old default)
	_, err = db.ExecContext(context.Background(), `INSERT INTO renovate_jobs (name) VALUES ('test-job')`)
	if err != nil {
		t.Fatalf("insert job failed: %v", err)
	}

	// Verify it has debug_mode=0
	var debugMode int
	err = db.QueryRowContext(context.Background(), `SELECT debug_mode FROM renovate_jobs WHERE name = 'test-job'`).Scan(&debugMode)
	if err != nil {
		t.Fatalf("query debug_mode failed: %v", err)
	}
	if debugMode != 0 {
		t.Fatalf("expected debug_mode=0 before migration, got %d", debugMode)
	}

	// Apply migration0002
	_, err = db.ExecContext(context.Background(), migration0002)
	if err != nil {
		t.Fatalf("migration0002 failed: %v", err)
	}

	// Verify existing job now has debug_mode=1
	err = db.QueryRowContext(context.Background(), `SELECT debug_mode FROM renovate_jobs WHERE name = 'test-job'`).Scan(&debugMode)
	if err != nil {
		t.Fatalf("query debug_mode after migration failed: %v", err)
	}
	if debugMode != 1 {
		t.Fatalf("expected debug_mode=1 after migration, got %d", debugMode)
	}

	// Insert a new job (should default to 1)
	_, err = db.ExecContext(context.Background(), `INSERT INTO renovate_jobs (name) VALUES ('new-job')`)
	if err != nil {
		t.Fatalf("insert new job failed: %v", err)
	}

	err = db.QueryRowContext(context.Background(), `SELECT debug_mode FROM renovate_jobs WHERE name = 'new-job'`).Scan(&debugMode)
	if err != nil {
		t.Fatalf("query new job debug_mode failed: %v", err)
	}
	if debugMode != 1 {
		t.Fatalf("expected new job debug_mode=1, got %d", debugMode)
	}

	_ = db.Close()
}

func TestSettings_GetSetRoundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Non-existent key returns not found.
	val, found, err := store.GetSetting(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetSetting(nonexistent) error: %v", err)
	}
	if found {
		t.Fatalf("expected not found, got value=%q", val)
	}

	// Insert a setting.
	if err := store.SetSetting(ctx, "test_key", "test_value"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	// Read it back.
	val, found, err = store.GetSetting(ctx, "test_key")
	if err != nil {
		t.Fatalf("GetSetting(test_key) error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if val != "test_value" {
		t.Fatalf("expected 'test_value', got %q", val)
	}

	// Upsert (update existing).
	if err := store.SetSetting(ctx, "test_key", "updated_value"); err != nil {
		t.Fatalf("SetSetting (upsert) failed: %v", err)
	}

	val, found, err = store.GetSetting(ctx, "test_key")
	if err != nil {
		t.Fatalf("GetSetting after upsert error: %v", err)
	}
	if !found || val != "updated_value" {
		t.Fatalf("expected 'updated_value', got found=%v val=%q", found, val)
	}
}

func TestEnsureWebhookSecret_AutoGeneration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
	t.Setenv("ROP_WEBHOOK_SECRET", "")

	store, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Should have exactly 1 auto-generated token.
	if len(store.webhookTokens) != 1 {
		t.Fatalf("expected 1 webhook token, got %d", len(store.webhookTokens))
	}

	// Token should be 40 hex chars.
	token := store.webhookTokens[0]
	if len(token) != 40 {
		t.Fatalf("expected 40-char token, got %d chars: %q", len(token), token)
	}
	for _, c := range token {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("token contains non-hex char: %q", token)
		}
	}

	// DB should have the same value.
	ctx := context.Background()
	dbVal, found, err := store.GetSetting(ctx, "webhook_secret")
	if err != nil {
		t.Fatalf("GetSetting error: %v", err)
	}
	if !found {
		t.Fatal("expected webhook_secret in settings table")
	}
	if dbVal != token {
		t.Fatalf("DB value %q != token %q", dbVal, token)
	}
}

func TestEnsureWebhookSecret_Persistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
	t.Setenv("ROP_WEBHOOK_SECRET", "")

	// First open: generates secret.
	store1, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("first New() failed: %v", err)
	}
	firstToken := store1.webhookTokens[0]
	_ = store1.Close()

	// Second open: should load same secret.
	store2, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("second New() failed: %v", err)
	}
	defer func() { _ = store2.Close() }()

	if len(store2.webhookTokens) != 1 {
		t.Fatalf("expected 1 token on re-open, got %d", len(store2.webhookTokens))
	}
	if store2.webhookTokens[0] != firstToken {
		t.Fatalf("token changed on re-open: %q vs %q", store2.webhookTokens[0], firstToken)
	}
}

func TestEnsureWebhookSecret_EnvvarOverride(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
	t.Setenv("ROP_WEBHOOK_SECRET", "custom-secret-value")

	store, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	if len(store.webhookTokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(store.webhookTokens))
	}
	if store.webhookTokens[0] != "custom-secret-value" {
		t.Fatalf("expected 'custom-secret-value', got %q", store.webhookTokens[0])
	}

	// DB should also have the override value.
	ctx := context.Background()
	dbVal, found, err := store.GetSetting(ctx, "webhook_secret")
	if err != nil {
		t.Fatalf("GetSetting error: %v", err)
	}
	if !found || dbVal != "custom-secret-value" {
		t.Fatalf("expected DB to have 'custom-secret-value', got found=%v val=%q", found, dbVal)
	}
}

func TestEnsureWebhookSecret_CommaSeparated(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
	t.Setenv("ROP_WEBHOOK_SECRET", "secret1,secret2,secret3")

	store, err := New(dbPath, slog.Default())
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	expected := []string{"secret1", "secret2", "secret3"}
	if len(store.webhookTokens) != len(expected) {
		t.Fatalf("expected %d tokens, got %d: %v", len(expected), len(store.webhookTokens), store.webhookTokens)
	}
	for i, exp := range expected {
		if store.webhookTokens[i] != exp {
			t.Fatalf("token[%d]: expected %q, got %q", i, exp, store.webhookTokens[i])
		}
	}
}

// TestUpdateProjectStatusBatched_ReschedulesNonRunning verifies that after a
// cron discovery cycle the batch-update call sets completed/failed projects back
// to 'scheduled' while leaving any running project untouched.
func TestUpdateProjectStatusBatched_ReschedulesNonRunning(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	job := &api.RenovateJob{Name: "default"}
	jobID := RenovateJobIdentifier{Name: "default"}

	// Seed four projects.
	_, err := store.ReconcileProjects(ctx, job, []string{
		"org/completed-repo",
		"org/failed-repo",
		"org/running-repo",
		"org/scheduled-repo",
	})
	if err != nil {
		t.Fatalf("ReconcileProjects failed: %v", err)
	}

	// Set individual statuses so we have a mix.
	statuses := map[string]api.RenovateProjectStatus{
		"org/completed-repo": api.JobStatusCompleted,
		"org/failed-repo":    api.JobStatusFailed,
		"org/running-repo":   api.JobStatusRunning,
		// "org/scheduled-repo" keeps the default 'scheduled' status from ReconcileProjects.
	}
	for name, status := range statuses {
		s := status // capture loop variable
		if err := store.UpdateProjectStatus(ctx, name, jobID, &RenovateStatusUpdate{Status: s}); err != nil {
			t.Fatalf("UpdateProjectStatus(%s) failed: %v", name, err)
		}
	}

	// Simulate what runScheduledCycle will do after the bug-fix:
	// set all non-running projects back to 'scheduled'.
	err = store.UpdateProjectStatusBatched(
		ctx,
		func(p api.ProjectStatus) bool {
			return p.Status != api.JobStatusRunning
		},
		jobID,
		&RenovateStatusUpdate{Status: api.JobStatusScheduled},
	)
	if err != nil {
		t.Fatalf("UpdateProjectStatusBatched failed: %v", err)
	}

	// Verify final states.
	expectedScheduled := []string{"org/completed-repo", "org/failed-repo", "org/scheduled-repo"}
	for _, name := range expectedScheduled {
		projects, err := store.GetProjectsByStatus(ctx, jobID, api.JobStatusScheduled)
		if err != nil {
			t.Fatalf("GetProjectsByStatus(scheduled) failed: %v", err)
		}
		found := false
		for _, p := range projects {
			if p.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q to be 'scheduled' after batch update, but it was not", name)
		}
	}

	// The running project must remain running.
	runningProjects, err := store.GetProjectsByStatus(ctx, jobID, api.JobStatusRunning)
	if err != nil {
		t.Fatalf("GetProjectsByStatus(running) failed: %v", err)
	}
	if len(runningProjects) != 1 {
		t.Fatalf("expected 1 running project, got %d", len(runningProjects))
	}
	if runningProjects[0].Name != "org/running-repo" {
		t.Fatalf("expected running project to be 'org/running-repo', got %q", runningProjects[0].Name)
	}
}
