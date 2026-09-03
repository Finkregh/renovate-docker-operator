package imageupdate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// --- Fakes ------------------------------------------------------------------

type fakeHead struct {
	// digestByTag returns the digest for a given tag; empty tag maps to
	// digestByTag[""] (i.e. the configured tag path with no explicit tag
	// override).
	digestByTag map[string]string
	err         error
	calls       []Reference
}

func (f *fakeHead) HeadManifest(_ context.Context, ref Reference) (string, error) {
	f.calls = append(f.calls, ref)
	if f.err != nil {
		return "", f.err
	}
	if d, ok := f.digestByTag[ref.Tag]; ok {
		return d, nil
	}
	return "", errors.New("no digest for tag " + ref.Tag)
}

type fakeTags struct {
	tags []string
	err  error
}

func (f *fakeTags) ListTags(_ context.Context, _ Reference) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tags, nil
}

type fakeDigester struct {
	digest string
	err    error
}

func (f *fakeDigester) LocalDigest(_ context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.digest, nil
}

// silentLogger throws away log output so tests aren't noisy.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- Tests ------------------------------------------------------------------

func TestPoller_DisabledReturnsNilAndDoesNotCall(t *testing.T) {
	head := &fakeHead{}
	tags := &fakeTags{}
	dig := &fakeDigester{}
	p := New(PollerConfig{Enabled: false, ConfiguredRef: "docker.io/renovate/renovate:latest"},
		head, tags, dig, silentLogger())

	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("Tick on disabled poller returned error: %v", err)
	}
	if len(head.calls) != 0 {
		t.Errorf("disabled poller called HeadManifest %d times, want 0", len(head.calls))
	}
}

func TestPoller_MarkDisabledSetsReadyTrue(t *testing.T) {
	p := New(PollerConfig{Enabled: false, ConfiguredRef: "docker.io/renovate/renovate:44.11.3"},
		&fakeHead{}, &fakeTags{}, &fakeDigester{}, silentLogger())
	p.MarkDisabled()
	got := p.Snapshot()
	if got.Enabled {
		t.Errorf("Enabled = true, want false")
	}
	if !got.Ready {
		t.Errorf("Ready = false, want true after MarkDisabled")
	}
	if got.Registry != "docker.io" || got.Repository != "renovate/renovate" || got.Tag != "44.11.3" {
		t.Errorf("parsed fields missing: %+v", got)
	}
}

func TestPoller_SemverBranchDetectsUpdate(t *testing.T) {
	head := &fakeHead{digestByTag: map[string]string{
		"44.11.3": "sha256:aaa",
		"44.12.0": "sha256:bbb",
	}}
	tags := &fakeTags{tags: []string{"44.11.3", "44.11.4", "44.12.0", "44.12.0-slim", "latest"}}
	// LocalDigest matches remote to isolate the semver branch.
	dig := &fakeDigester{digest: "sha256:aaa"}

	p := New(PollerConfig{Enabled: true, ConfiguredRef: "docker.io/renovate/renovate:44.11.3"},
		head, tags, dig, silentLogger())
	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	got := p.Snapshot()
	if !got.Ready {
		t.Errorf("Ready = false, want true")
	}
	if !got.UpdateAvailable {
		t.Errorf("UpdateAvailable = false, want true (semver branch)")
	}
	if got.LatestTag != "44.12.0" {
		t.Errorf("LatestTag = %q, want %q", got.LatestTag, "44.12.0")
	}
	if got.LatestDigest != "sha256:bbb" {
		t.Errorf("LatestDigest = %q, want %q", got.LatestDigest, "sha256:bbb")
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}
}

func TestPoller_SemverBranchRespectsFamily(t *testing.T) {
	// Configured is a `-slim` tag; a higher pure semver tag must NOT
	// trigger an update banner.
	head := &fakeHead{digestByTag: map[string]string{
		"44.11.3-slim": "sha256:slim",
	}}
	tags := &fakeTags{tags: []string{"44.11.3-slim", "44.12.0", "44.13.0"}}
	dig := &fakeDigester{digest: "sha256:slim"}

	p := New(PollerConfig{Enabled: true, ConfiguredRef: "docker.io/renovate/renovate:44.11.3-slim"},
		head, tags, dig, silentLogger())
	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	got := p.Snapshot()
	if got.UpdateAvailable {
		t.Errorf("UpdateAvailable = true, want false (no -slim tag newer than configured)")
	}
	if got.LatestTag != "" {
		t.Errorf("LatestTag = %q, want empty", got.LatestTag)
	}
}

func TestPoller_DigestDriftDetectsUpdate(t *testing.T) {
	// Tag is not semver (e.g. `latest`), so only the digest branch
	// fires. Remote digest differs from local.
	head := &fakeHead{digestByTag: map[string]string{
		"latest": "sha256:remote",
	}}
	tags := &fakeTags{} // not consulted for non-semver tags
	dig := &fakeDigester{digest: "sha256:local-stale"}

	p := New(PollerConfig{Enabled: true, ConfiguredRef: "docker.io/renovate/renovate:latest"},
		head, tags, dig, silentLogger())
	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	got := p.Snapshot()
	if !got.UpdateAvailable {
		t.Errorf("UpdateAvailable = false, want true (digest drift)")
	}
	if got.LatestTag != "" {
		t.Errorf("LatestTag = %q, want empty (digest branch only)", got.LatestTag)
	}
	if got.Digest != "sha256:remote" || got.LocalDigest != "sha256:local-stale" {
		t.Errorf("digest fields not populated: %+v", got)
	}
}

func TestPoller_ColdCacheSuppressesDigestBranch(t *testing.T) {
	// LocalDigest = "" means image not pulled yet (first boot). The
	// digest branch must NOT fire — no "update available" banner
	// before we've even pulled once.
	head := &fakeHead{digestByTag: map[string]string{
		"latest": "sha256:remote",
	}}
	tags := &fakeTags{}
	dig := &fakeDigester{digest: ""}

	p := New(PollerConfig{Enabled: true, ConfiguredRef: "docker.io/renovate/renovate:latest"},
		head, tags, dig, silentLogger())
	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	got := p.Snapshot()
	if got.UpdateAvailable {
		t.Errorf("UpdateAvailable = true on cold cache; want false (no local digest to compare)")
	}
	if !got.Ready {
		t.Errorf("Ready = false, want true (a successful HEAD counts as ready)")
	}
}

func TestPoller_TotalFailurePreservesPriorStateExceptError(t *testing.T) {
	// First tick succeeds; second tick errors on HEAD.
	head := &fakeHead{digestByTag: map[string]string{"latest": "sha256:remote"}}
	dig := &fakeDigester{digest: "sha256:remote"}
	p := New(PollerConfig{Enabled: true, ConfiguredRef: "docker.io/renovate/renovate:latest"},
		head, &fakeTags{}, dig, silentLogger())
	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("first tick failed: %v", err)
	}
	priorDigest := p.Snapshot().Digest
	if priorDigest == "" {
		t.Fatalf("digest not populated after first tick")
	}

	// Now fail the second tick.
	head.err = errors.New("boom")
	if err := p.Tick(context.Background()); err == nil {
		t.Fatalf("second tick expected to error, got nil")
	}
	got := p.Snapshot()
	if got.Digest != priorDigest {
		t.Errorf("digest cleared after total failure: got %q, want %q (last-known-good preserved)", got.Digest, priorDigest)
	}
	if got.LastError == "" {
		t.Errorf("LastError empty after failed tick")
	}
}

func TestPoller_PartialSuccessKeepsRemoteDigestOnListTagsFailure(t *testing.T) {
	// HEAD succeeds; ListTags fails. Expect remote digest published,
	// no LatestTag, LastError set.
	head := &fakeHead{digestByTag: map[string]string{"44.11.3": "sha256:remote"}}
	tags := &fakeTags{err: errors.New("timeout")}
	dig := &fakeDigester{digest: "sha256:remote"}
	p := New(PollerConfig{Enabled: true, ConfiguredRef: "docker.io/renovate/renovate:44.11.3"},
		head, tags, dig, silentLogger())
	if err := p.Tick(context.Background()); err == nil {
		t.Fatalf("expected classified error from partial-success tick")
	}
	got := p.Snapshot()
	if got.Digest != "sha256:remote" {
		t.Errorf("Digest = %q, want sha256:remote", got.Digest)
	}
	if got.LatestTag != "" {
		t.Errorf("LatestTag = %q, want empty (ListTags failed)", got.LatestTag)
	}
	if !got.Ready {
		t.Errorf("Ready = false, want true (partial success still counts)")
	}
	if got.LastError == "" {
		t.Errorf("LastError empty after partial-success tick")
	}
}

func TestPoller_UnparseableRefErrorsPerTick(t *testing.T) {
	p := New(PollerConfig{Enabled: true, ConfiguredRef: "%%invalid%%"},
		&fakeHead{}, &fakeTags{}, &fakeDigester{}, silentLogger())
	if err := p.Tick(context.Background()); err == nil {
		t.Fatalf("expected error for unparseable ref")
	}
	got := p.Snapshot()
	if got.LastError == "" {
		t.Errorf("LastError empty")
	}
	// Ready stays false — we've observed nothing about the remote.
	if got.Ready {
		t.Errorf("Ready = true, want false (nothing observed)")
	}
}

func TestPoller_TruncateError(t *testing.T) {
	long := ""
	for i := 0; i < 500; i++ {
		long += "x"
	}
	got := truncateError(long, 120)
	// 120 + one "…" rune ("…" is 3 UTF-8 bytes; len returns bytes)
	if len(got) != 120+len("…") {
		t.Errorf("len = %d, want %d", len(got), 120+len("…"))
	}
	if !hasSuffix(got, "…") {
		t.Errorf("got %q, want trailing ellipsis", got)
	}

	multiline := "first line\nsecond line"
	got2 := truncateError(multiline, 120)
	if got2 != "first line" {
		t.Errorf("got %q, want %q", got2, "first line")
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
