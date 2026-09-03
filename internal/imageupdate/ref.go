// Package imageupdate polls a container registry to detect updates for the
// configured Renovate image. It publishes a snapshot of the latest observation
// via Snapshot(), consumed by the HTTP /api/v1/image-status endpoint.
//
// v1 scope: anonymous polling only. Bearer-token challenges are handled
// registry-agnostically (any realm the registry declares), but no static
// credentials (~/.docker/config.json, credHelpers, env vars) are consulted.
// Private registries need a follow-up plan.
package imageupdate

import (
	"errors"
	"fmt"
	"strings"
)

// Reference is a parsed OCI image reference.
type Reference struct {
	// Registry is the DNS host of the registry, e.g. "index.docker.io",
	// "ghcr.io", or "registry.example.com:5000". Never empty after Parse.
	Registry string
	// Repository is the path within the registry, e.g. "renovate/renovate"
	// or "library/nginx" (bare Docker Hub images are namespaced under
	// "library/"). Never empty after Parse.
	Repository string
	// Tag is the human tag, e.g. "latest" or "44.11.3". Empty iff the ref
	// is pinned by digest (an "@sha256:..." suffix with no ":" tag).
	Tag string
	// Digest is the "sha256:..." pin, empty iff pinned by tag.
	Digest string
}

// DefaultRegistry is used when the reference has no explicit registry host.
const DefaultRegistry = "index.docker.io"

// ErrEmptyRef is returned by Parse for the empty string.
var ErrEmptyRef = errors.New("imageupdate: empty image reference")

// Parse converts an OCI image reference string into a Reference.
//
// It mirrors a minimal subset of the containers/image and docker reference
// grammars, sufficient for the poller's needs:
//
//   - Registry host is the portion before the first "/" only if it contains
//     ".", ":", or equals "localhost". Otherwise the registry defaults to
//     index.docker.io and the whole string is repository+tag.
//   - Bare Docker Hub repositories ("foo") are expanded to "library/foo".
//   - "@sha256:..." after the repository/tag pins by digest.
//   - ":tag" after the repository names a tag. Default tag is "latest" if
//     neither "@" nor ":" is present after the repository segment.
//   - The port on a host (":5000") is disambiguated from a tag by the
//     presence of a subsequent "/" — hosts with ports always have a path.
//
// Parse does NOT validate that the repository/tag characters are legal per
// the OCI spec — it accepts anything printable. The registry client will
// surface invalid characters as a 400 from the registry.
func Parse(ref string) (Reference, error) {
	if ref == "" {
		return Reference{}, ErrEmptyRef
	}

	// Split off digest first — @sha256:... is unambiguous.
	var digest string
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		digest = ref[at+1:]
		ref = ref[:at]
		if digest == "" {
			return Reference{}, fmt.Errorf("imageupdate: empty digest in reference")
		}
	}

	// Decide whether the first slash-segment is a registry host. It is iff
	// it contains ".", ":", or equals "localhost".
	var registry, remainder string
	if slash := strings.Index(ref, "/"); slash >= 0 {
		first := ref[:slash]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			registry = first
			remainder = ref[slash+1:]
		}
	}
	if registry == "" {
		registry = DefaultRegistry
		remainder = ref
	}

	if remainder == "" {
		return Reference{}, fmt.Errorf("imageupdate: empty repository in reference")
	}

	// Split remainder into repo and tag. The tag separator is the LAST ":"
	// after the last "/", or the whole string has no "/" and no ":" past
	// the (already-stripped) host.
	var repository, tag string
	if colon := strings.LastIndex(remainder, ":"); colon >= 0 && !strings.Contains(remainder[colon+1:], "/") {
		repository = remainder[:colon]
		tag = remainder[colon+1:]
		if tag == "" {
			return Reference{}, fmt.Errorf("imageupdate: empty tag in reference")
		}
	} else {
		repository = remainder
	}

	if repository == "" {
		return Reference{}, fmt.Errorf("imageupdate: empty repository in reference")
	}

	// Docker Hub: bare repositories are namespaced under "library/".
	if registry == DefaultRegistry && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}

	// Default tag is "latest" only when there is no digest pin.
	if tag == "" && digest == "" {
		tag = "latest"
	}

	return Reference{
		Registry:   registry,
		Repository: repository,
		Tag:        tag,
		Digest:     digest,
	}, nil
}
