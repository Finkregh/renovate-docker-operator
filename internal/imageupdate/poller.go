package imageupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Status is the JSON-serializable snapshot published by Snapshot() and
// served over /api/v1/image-status.
//
// All fields carry explicit `json:"<camelCase>"` tags because the
// frontend accesses them as camelCase (imageStatus.configuredRef,
// imageStatus.updateAvailable, etc.).
//
// Semantics:
//   - Enabled == false means the poller is disabled via
//     ROP_CHECK_UPDATE_RUNNER_IMAGE=false. Ready is still true (via
//     MarkDisabled) so callers see a stable "no data, by design" shape.
//   - Ready == true iff either MarkDisabled has been called OR at least
//     one Tick has completed (successfully or with LastError set). Once
//     true, Ready never reverts.
//   - LastError is the last poll error message ("" on success). A tick
//     that fails after a successful earlier tick still leaves Ready=true
//     and LastError populated — the semantic is "has this ever been
//     observed," not "is the last observation fresh."
type Status struct {
	Enabled         bool      `json:"enabled"`
	Ready           bool      `json:"ready"`
	ConfiguredRef   string    `json:"configuredRef"`
	Registry        string    `json:"registry"`
	Repository      string    `json:"repository"`
	Tag             string    `json:"tag"`
	Digest          string    `json:"digest,omitempty"`
	LocalDigest     string    `json:"localDigest,omitempty"`
	LatestTag       string    `json:"latestTag,omitempty"`
	LatestDigest    string    `json:"latestDigest,omitempty"`
	UpdateAvailable bool      `json:"updateAvailable"`
	LastChecked     time.Time `json:"lastChecked,omitempty"`
	LastError       string    `json:"lastError,omitempty"`
}

// LocalDigester is the narrow executor dependency the Poller needs.
// *executor.DockerExecutor satisfies it; tests use a hand-rolled fake.
type LocalDigester interface {
	LocalDigest(ctx context.Context) (string, error)
}

// HeadManifester and TagLister are the narrow registry-client
// dependencies the Poller needs. *Client (registry.go) satisfies both;
// tests can inject a fake without spinning up an httptest.Server for
// non-HTTP concerns.
type HeadManifester interface {
	HeadManifest(ctx context.Context, ref Reference) (string, error)
}

type TagLister interface {
	ListTags(ctx context.Context, ref Reference) ([]string, error)
}

// PollerConfig is the subset of config.Config the Poller reads. Passed
// as a plain struct so this package doesn't import the top-level
// config package (which would create a cycle once config.Validate calls
// imageupdate.Parse).
type PollerConfig struct {
	Enabled       bool   // ROP_CHECK_UPDATE_RUNNER_IMAGE
	ConfiguredRef string // ROP_IMAGE
}

// Poller drives per-tick registry checks and publishes a Snapshot.
type Poller struct {
	cfg    PollerConfig
	logger *slog.Logger

	head   HeadManifester
	tags   TagLister
	digest LocalDigester

	// parsed carries the ConfiguredRef parsed at construction time;
	// populated once, never mutated after New.
	parsed Reference

	mu    sync.RWMutex
	state Status
}

// New constructs a Poller. Parses cfg.ConfiguredRef eagerly and
// pre-populates the state with Registry/Repository/Tag so /image-status
// can return the parse-time fields before the first tick lands.
//
// A parse error is NOT fatal here — the constructor accepts anything.
// Fatal-at-boot validation lives in config.Validate (called from
// cmd/main.go) so tests and future callers stay flexible.
func New(cfg PollerConfig, head HeadManifester, tags TagLister, digest LocalDigester, logger *slog.Logger) *Poller {
	p := &Poller{
		cfg:    cfg,
		logger: logger.With(slog.String("component", "imageupdate")),
		head:   head,
		tags:   tags,
		digest: digest,
		state: Status{
			Enabled:       cfg.Enabled,
			ConfiguredRef: cfg.ConfiguredRef,
		},
	}
	if ref, err := Parse(cfg.ConfiguredRef); err == nil {
		p.parsed = ref
		p.state.Registry = ref.Registry
		p.state.Repository = ref.Repository
		p.state.Tag = ref.Tag
	} else {
		// Log at construction; supervise.Loop will observe a per-tick
		// error too, but this makes the boot-time misconfiguration
		// visible even before the first tick runs.
		p.logger.Warn("image reference did not parse; poller will report errors per tick",
			slog.String("ref", cfg.ConfiguredRef),
			slog.String("error", err.Error()),
		)
	}
	return p
}

// Snapshot returns a value copy of the current state under the read
// lock. Callers (HTTP handler) never see a shared pointer and cannot
// race a concurrent Tick.
func (p *Poller) Snapshot() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// MarkDisabled sets Enabled=false, Ready=true synchronously. Called
// from cmd/main.go when ROP_CHECK_UPDATE_RUNNER_IMAGE=false so
// Snapshot() returns a stable "no data, by design" shape from the
// first request.
func (p *Poller) MarkDisabled() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = Status{
		Enabled:       false,
		Ready:         true,
		ConfiguredRef: p.cfg.ConfiguredRef,
		Registry:      p.parsed.Registry,
		Repository:    p.parsed.Repository,
		Tag:           p.parsed.Tag,
	}
}

// Tick performs one registry poll. Returns a classified error on
// failure; supervise.Loop logs it and continues to the next interval.
// Never panics on business-logic errors; panics on unexpected
// programmer errors (nil dependencies) which supervise.Loop treats as
// fatal via the recovered-panic-is-fatal contract.
func (p *Poller) Tick(ctx context.Context) error {
	if !p.cfg.Enabled {
		return nil // disabled poller: caller should never invoke Tick
	}
	if p.parsed.Registry == "" {
		return p.recordTotalFailure(fmt.Errorf("image reference %q not parseable", p.cfg.ConfiguredRef))
	}

	next := p.Snapshot()
	next.Enabled = true

	// (2) HEAD current → remote digest.
	remoteDigest, err := p.head.HeadManifest(ctx, p.parsed)
	if err != nil {
		return p.recordTotalFailure(err)
	}
	next.Digest = remoteDigest

	// (3) Local digest — decoupled from remote HEAD. Nil digester in
	// tests skips this branch.
	if p.digest != nil {
		local, err := p.digest.LocalDigest(ctx)
		if err != nil {
			// Local-digest error is a partial failure: we still have a
			// remote digest and possibly a semver tag observation.
			return p.recordPartialSuccess(next, fmt.Errorf("local digest: %w", err))
		}
		next.LocalDigest = local
	}

	// (4) Semver branch — only when configured tag is X.Y.Z or
	// X.Y.Z<suffix>.
	curMaj, curMi, curPa, curSuffix, curOK := parseSemverPrefix(p.parsed.Tag)
	if curOK {
		tags, err := p.tags.ListTags(ctx, p.parsed)
		if err != nil {
			// Partial: keep remote/local digest, drop semver branch.
			return p.recordPartialSuccess(next, fmt.Errorf("list tags: %w", err))
		}
		if latest, ok := LatestSemverTag(tags, curSuffix); ok {
			lMaj, lMi, lPa, _, _ := parseSemverPrefix(latest)
			// Only report if strictly newer than the configured tag.
			if lMaj > curMaj ||
				(lMaj == curMaj && lMi > curMi) ||
				(lMaj == curMaj && lMi == curMi && lPa > curPa) {
				next.LatestTag = latest
				// HEAD the latest tag to get its digest.
				latestRef := p.parsed
				latestRef.Tag = latest
				latestRef.Digest = ""
				if ld, err := p.head.HeadManifest(ctx, latestRef); err == nil {
					next.LatestDigest = ld
				}
				// Do NOT bail on error here — semver-branch banner
				// works without the digest.
			}
		}
	}

	// (5) UpdateAvailable — either branch fires.
	next.UpdateAvailable = false
	if next.LatestTag != "" {
		next.UpdateAvailable = true
	}
	if next.LocalDigest != "" && next.Digest != "" && next.LocalDigest != next.Digest {
		next.UpdateAvailable = true
	}

	next.Ready = true
	next.LastChecked = time.Now()
	next.LastError = ""

	p.mu.Lock()
	p.state = next
	p.mu.Unlock()
	return nil
}

// recordTotalFailure updates state.LastError and state.LastChecked
// without touching the other fields (last-known-good preservation).
// Returns the classified error so supervise.Loop can log it.
func (p *Poller) recordTotalFailure(err error) error {
	classified := err
	var already *ClassifiedError
	if !errors.As(err, &already) {
		classified = classifyErr(ClassNetwork, err)
	}
	msg := classified.Error()

	p.mu.Lock()
	// Preserve prior state; overwrite only LastError / LastChecked.
	p.state.LastError = truncateError(msg, 120)
	p.state.LastChecked = time.Now()
	// A total-failure tick does NOT flip Ready to true. Ready flips
	// only when we observe *something* concrete about the remote (per
	// plan §C4: "the endpoint's meaning is 'has this been observed at
	// least once'").
	p.mu.Unlock()

	return classified
}

// recordPartialSuccess installs a "next" that has some successful
// branches populated, sets LastError, marks Ready=true, and swaps.
// Returns the classified error for supervise.Loop.
func (p *Poller) recordPartialSuccess(next Status, err error) error {
	classified := err
	var already *ClassifiedError
	if !errors.As(err, &already) {
		classified = classifyErr(ClassNetwork, err)
	}

	next.Ready = true
	next.LastChecked = time.Now()
	next.LastError = truncateError(classified.Error(), 120)

	// Preserve stale branches from prior state that we didn't touch.
	prior := p.Snapshot()
	if next.LatestTag == "" && prior.LatestTag != "" {
		next.LatestTag = prior.LatestTag
		next.LatestDigest = prior.LatestDigest
	}
	// UpdateAvailable is derived from whatever fields we now hold.
	next.UpdateAvailable = false
	if next.LatestTag != "" {
		next.UpdateAvailable = true
	}
	if next.LocalDigest != "" && next.Digest != "" && next.LocalDigest != next.Digest {
		next.UpdateAvailable = true
	}

	p.mu.Lock()
	p.state = next
	p.mu.Unlock()
	return classified
}

// truncateError shortens an error string for the UI tooltip. Format:
// keep the first line, cap at max runes, append "…" on truncation.
func truncateError(msg string, max int) string {
	// First-line only.
	for i := 0; i < len(msg); i++ {
		if msg[i] == '\n' {
			msg = msg[:i]
			break
		}
	}
	if len(msg) <= max {
		return msg
	}
	return msg[:max] + "…"
}
