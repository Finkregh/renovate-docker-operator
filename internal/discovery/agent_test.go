package discovery

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
)

func TestNew_NilLogger(t *testing.T) {
	// Passing nil logger should not panic and should default to slog.Default().
	agent := New(nil, nil, nil)
	if agent == nil {
		t.Fatal("expected non-nil agent")
	}
	if agent.logger == nil {
		t.Fatal("expected agent.logger to be set to default, got nil")
	}
}

func TestNew_CustomLogger(t *testing.T) {
	customLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	agent := New(nil, nil, customLogger)
	if agent == nil {
		t.Fatal("expected non-nil agent")
	}
	if agent.logger != customLogger {
		t.Fatal("expected agent.logger to match provided custom logger")
	}
}

func TestNew_NilExecutorAndStore(t *testing.T) {
	// A nil executor/store is valid at construction time; only fails when RunDiscovery is called.
	agent := New(nil, nil, slog.Default())
	if agent == nil {
		t.Fatal("expected non-nil agent")
	}
	if agent.executor != nil {
		t.Fatal("expected agent.executor to be nil")
	}
	if agent.store != nil {
		t.Fatal("expected agent.store to be nil")
	}
}

func TestRunDiscovery_NilExecutor_Panics(t *testing.T) {
	// When executor is nil, calling RunDiscovery should panic with a nil pointer dereference.
	agent := New(nil, nil, slog.Default())
	job := &api.RenovateJob{Name: "test-job"}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic from nil executor, but RunDiscovery returned normally")
		}
	}()

	_, _ = agent.RunDiscovery(context.Background(), job)
}

func TestRunDiscovery_CancelledContext_NilExecutor_Panics(t *testing.T) {
	// Even with a cancelled context, nil executor still panics because the method
	// dereferences the executor pointer before checking context.
	agent := New(nil, nil, slog.Default())
	job := &api.RenovateJob{Name: "test-job"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic from nil executor with cancelled context")
		}
	}()

	_, _ = agent.RunDiscovery(ctx, job)
}
