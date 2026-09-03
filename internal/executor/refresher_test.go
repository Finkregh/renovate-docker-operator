package executor

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
)

// fakePuller is a hand-rolled test double for the imagePuller interface.
// It records every ImagePull call and can be scripted to return errors or
// specific readers.
type fakePuller struct {
	calls   atomic.Int32
	lastRef atomic.Value // string
	err     error
	body    string // stream body drained by refreshImageWith
}

func (f *fakePuller) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	f.calls.Add(1)
	f.lastRef.Store(ref)
	if f.err != nil {
		return nil, f.err
	}
	body := f.body
	if body == "" {
		body = `{"status":"Pulling"}`
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

// fakeCache captures markImageChecked invocations.
type fakeCache struct {
	calls atomic.Int32
	last  atomic.Value // string ref
	at    atomic.Value // time.Time
}

func (c *fakeCache) markImageChecked(ref string, at time.Time) {
	c.calls.Add(1)
	c.last.Store(ref)
	c.at.Store(at)
}

func TestRefreshImage_CallsImagePullUnconditionally(t *testing.T) {
	puller := &fakePuller{}
	cache := &fakeCache{}

	if err := refreshImageWith(context.Background(), puller, cache, "renovate/renovate:latest"); err != nil {
		t.Fatalf("refreshImageWith: unexpected error: %v", err)
	}

	if got := puller.calls.Load(); got != 1 {
		t.Errorf("ImagePull call count = %d, want 1", got)
	}
	if got := puller.lastRef.Load().(string); got != "renovate/renovate:latest" {
		t.Errorf("ImagePull ref = %q, want %q", got, "renovate/renovate:latest")
	}
	if got := cache.calls.Load(); got != 1 {
		t.Errorf("markImageChecked call count = %d, want 1", got)
	}
	if got := cache.last.Load().(string); got != "renovate/renovate:latest" {
		t.Errorf("cache last ref = %q, want %q", got, "renovate/renovate:latest")
	}
}

func TestRefreshImage_PropagatesPullError(t *testing.T) {
	sentinel := errors.New("registry timeout")
	puller := &fakePuller{err: sentinel}
	cache := &fakeCache{}

	err := refreshImageWith(context.Background(), puller, cache, "renovate/renovate:latest")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error not wrapping sentinel: %v", err)
	}
	if got := cache.calls.Load(); got != 0 {
		t.Errorf("cache updated on error (calls=%d), want 0", got)
	}
}

func TestRefreshImage_RespectsContextCancellation(t *testing.T) {
	// Direct method test — bypasses refreshImageWith and hits the ctx.Err()
	// short-circuit on the receiver method itself. We use a real
	// *DockerExecutor with a nil docker client because the ctx.Err() branch
	// returns before touching e.docker.
	e := &DockerExecutor{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := e.RefreshImage(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshImage(cancelled ctx) = %v, want context.Canceled", err)
	}
}

// errorReader is a ReadCloser that fails mid-stream to exercise the
// drain-error branch.
type errorReader struct{ err error }

func (r *errorReader) Read([]byte) (int, error) { return 0, r.err }
func (r *errorReader) Close() error             { return nil }

type errorPuller struct{ err error }

func (p *errorPuller) ImagePull(context.Context, string, image.PullOptions) (io.ReadCloser, error) {
	return &errorReader{err: p.err}, nil
}

func TestRefreshImage_DrainError_IsTransient(t *testing.T) {
	sentinel := errors.New("stream reset")
	cache := &fakeCache{}

	err := refreshImageWith(context.Background(), &errorPuller{err: sentinel}, cache, "renovate/renovate:latest")
	if err == nil {
		t.Fatal("expected drain error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("drain error not wrapping sentinel: %v", err)
	}
	if got := cache.calls.Load(); got != 0 {
		t.Errorf("cache updated on drain error (calls=%d), want 0", got)
	}
}

func TestMarkImageChecked_UpdatesCache(t *testing.T) {
	e := &DockerExecutor{
		imageCache: make(map[string]time.Time),
	}
	now := time.Now()
	e.markImageChecked("renovate/renovate:latest", now)

	e.mu.Lock()
	got, ok := e.imageCache["renovate/renovate:latest"]
	e.mu.Unlock()

	if !ok {
		t.Fatal("imageCache entry missing")
	}
	if !got.Equal(now) {
		t.Errorf("imageCache timestamp = %v, want %v", got, now)
	}
}
