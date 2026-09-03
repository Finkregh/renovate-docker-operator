package executor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// imagePuller is the narrow subset of the Docker client used by RefreshImage.
// Kept unexported and package-local so we can substitute a fake in tests
// without exposing a broader interface than the refresher needs.
type imagePuller interface {
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
}

// RefreshImage unconditionally pulls the configured image, drains the pull
// response, and updates the internal image-cache timestamp. It is intended to
// be driven by supervise.Loop from cmd/main.go on the ROP_IMAGE_CACHE_TTL
// cadence.
//
// Contract:
//   - Always calls ImagePull (never short-circuits on cache or ImageInspect).
//   - Updates imageCache[e.image] on success only.
//   - Propagates the underlying ImagePull error verbatim (wrapped for context)
//     so supervise.Loop can log-and-continue.
//   - Respects ctx cancellation: if ctx is already done, returns ctx.Err()
//     without calling the Docker API.
//   - Panic-free: no defer/recover here. The fatal-on-panic invariant is
//     the supervise.Loop caller's responsibility.
//
// Not a replacement for pullImage; pullImage still runs on the dispatch hot
// path as the "image missing locally" safety net. The background refresher
// keeps the local image current so pullImage's ImageInspect short-circuit
// almost always wins.
func (e *DockerExecutor) RefreshImage(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return refreshImageWith(ctx, e.docker, e, e.image)
}

// refreshImageWith is the pull loop split off for injectable testing. It
// takes an imagePuller and a cache-updater seam so a fake client can drive
// the exact same code the production path runs.
func refreshImageWith(ctx context.Context, puller imagePuller, cache cacheUpdater, ref string) error {
	reader, err := puller.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("refresh image %s: %w", ref, err)
	}
	defer func() { _ = reader.Close() }()

	if _, err := io.Copy(io.Discard, reader); err != nil {
		// Copy errors here mean the stream closed mid-drain. The pull may
		// have partially completed; treat it as a transient failure so
		// supervise.Loop retries on the next tick.
		return fmt.Errorf("drain image pull %s: %w", ref, err)
	}

	cache.markImageChecked(ref, time.Now())
	return nil
}

// cacheUpdater is the narrow seam refreshImageWith uses to record a
// successful pull without depending on the full DockerExecutor surface.
type cacheUpdater interface {
	markImageChecked(ref string, at time.Time)
}

// markImageChecked updates imageCache[ref] under the mutex. Safe to call
// even when imageCacheTTL == 0 — the map is always initialized and the
// timestamp is used by both the hot-path short-circuit (when TTL > 0) and
// future observability endpoints.
func (e *DockerExecutor) markImageChecked(ref string, at time.Time) {
	e.mu.Lock()
	e.imageCache[ref] = at
	e.mu.Unlock()
}

// LocalDigest returns the first RepoDigest of the configured image as
// present on the local Docker daemon, or "" if the image is not yet
// pulled. Consumed by internal/imageupdate.Poller to compare against the
// remote registry HEAD (mutable-tag digest-drift detection).
//
// Errors from ImageInspect other than "not found" are propagated; "not
// found" is normalized to ("", nil) so the poller can distinguish
// "first-boot cold cache" from "docker daemon unreachable".
func (e *DockerExecutor) LocalDigest(ctx context.Context) (string, error) {
	info, err := e.docker.ImageInspect(ctx, e.image)
	if err != nil {
		if client.IsErrNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect local image %s: %w", e.image, err)
	}
	if len(info.RepoDigests) == 0 {
		return "", nil
	}
	// RepoDigests are "repo@sha256:..." strings; we return only the
	// digest portion so callers can compare directly against the value
	// from Docker-Content-Digest response headers.
	first := info.RepoDigests[0]
	if at := strings.LastIndex(first, "@"); at >= 0 {
		return first[at+1:], nil
	}
	return first, nil
}
