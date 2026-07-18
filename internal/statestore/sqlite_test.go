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
