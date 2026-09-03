package imageupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxManifestBody caps manifest / tags-list / token-realm response bodies
// at 1 MiB. Defensive against hostile or misconfigured registries — a real
// manifest is a few KiB, tokens are a few hundred bytes, and tags-list
// pages are typically a few KiB per page.
const maxManifestBody = 1 << 20 // 1 MiB

// headManifestTimeout is the per-call context timeout for HEAD/GET calls
// against the registry itself.
const headManifestTimeout = 30 * time.Second

// tokenRealmTimeout is the per-call context timeout for the bearer-token
// realm exchange (a separate HTTP roundtrip from the registry proper).
const tokenRealmTimeout = 15 * time.Second

// responseHeaderTimeout caps the time between "TCP connection accepted"
// and "response headers received". Prevents a hostile registry from
// holding the connection open indefinitely without ever writing a
// response.
const responseHeaderTimeout = 20 * time.Second

// manifestAccept is the value of the Accept header for HEAD manifest
// requests — covers OCI image index, docker manifest list v2, OCI image
// manifest, and docker manifest v2. Order matters: the registry
// content-negotiates against the first supported type in the list.
const manifestAccept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// Client is a Registry v2 client. Construct once at process start;
// concurrent use is safe (the underlying http.Client is safe).
type Client struct {
	http *http.Client
	auth Authenticator
	// userAgent is sent on every request. Populated at construction time
	// from the operator's version string.
	userAgent string
}

// NewClient constructs a Client with a hardened http.Transport:
//
//   - ResponseHeaderTimeout=20s (all other DefaultTransport settings
//     preserved: keep-alives, IdleConnTimeout=90s, MaxIdleConnsPerHost=2,
//     TLSHandshakeTimeout=10s, system root CA bundle).
//   - CheckRedirect strips the Authorization header when the redirect
//     crosses hosts — prevents bearer-token leakage to a CDN blob URL.
//   - No http.Client.Timeout — all timeout budgets come from
//     context.WithTimeout on each call, so supervise.Loop's tick context
//     propagates cleanly.
func NewClient(userAgent string, auth Authenticator) *Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Defensive: unknown DefaultTransport type. Fall back to a bare
		// http.Transport{} with just our timeout. This branch is not hit
		// on any current stdlib we support.
		base = &http.Transport{}
	}
	tr := base.Clone()
	tr.ResponseHeaderTimeout = responseHeaderTimeout

	c := &Client{
		http: &http.Client{
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				// Cross-host redirect: strip Authorization so the bearer
				// token doesn't leak to the redirect target.
				if len(via) > 0 && req.URL.Host != via[0].URL.Host {
					req.Header.Del("Authorization")
				}
				return nil
			},
		},
		auth:      auth,
		userAgent: userAgent,
	}
	return c
}

// HeadManifest issues HEAD against /v2/<repo>/manifests/<tag-or-digest>
// and returns the value of the Docker-Content-Digest response header.
//
// On 401, parses the WWW-Authenticate: Bearer challenge, fetches an
// anonymous token from the realm (via c.auth), and retries the HEAD
// once. Failing that, returns a classified error.
//
// The per-call timeout is 30s; callers may pass a shorter deadline via
// ctx.
func (c *Client) HeadManifest(ctx context.Context, ref Reference) (string, error) {
	if err := ref.validForHTTP(); err != nil {
		return "", err
	}
	target := ref.Tag
	if target == "" {
		target = ref.Digest
	}
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Registry, ref.Repository, target)

	ctx, cancel := context.WithTimeout(ctx, headManifestTimeout)
	defer cancel()

	digest, err := c.doHeadManifest(ctx, url, "")
	if err == nil {
		return digest, nil
	}

	var chal *bearerChallenge
	if errors.As(err, &chal) && c.auth != nil {
		token, tokErr := c.auth.AnonymousBearer(ctx, chal.challenge)
		if tokErr != nil {
			return "", classifyErr("auth", fmt.Errorf("bearer token exchange: %w", tokErr))
		}
		return c.doHeadManifest(ctx, url, token)
	}
	return "", err
}

// doHeadManifest is the single-shot HEAD implementation. token, when
// non-empty, is used as "Authorization: Bearer <token>".
func (c *Client) doHeadManifest(ctx context.Context, url, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", classifyErr("parse", fmt.Errorf("build HEAD request: %w", err))
	}
	req.Header.Set("Accept", manifestAccept)
	req.Header.Set("User-Agent", c.userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", classifyErr("network", fmt.Errorf("HEAD %s: %w", url, err))
	}
	// HEAD response: no body to drain, but close the connection tracker.
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		d := resp.Header.Get("Docker-Content-Digest")
		if d == "" {
			return "", classifyErr("parse", fmt.Errorf("HEAD %s: missing Docker-Content-Digest header", url))
		}
		return d, nil
	case http.StatusUnauthorized:
		challenge := resp.Header.Get("WWW-Authenticate")
		if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
			return "", classifyErr("auth", fmt.Errorf("HEAD %s: 401 without Bearer challenge (header=%q)", url, challenge))
		}
		return "", &bearerChallenge{challenge: challenge}
	case http.StatusNotFound:
		return "", classifyErr("not_found", fmt.Errorf("HEAD %s: 404 (image or tag missing)", url))
	default:
		return "", classifyErr("network", fmt.Errorf("HEAD %s: unexpected status %d", url, resp.StatusCode))
	}
}

// ListTags returns every tag on the referenced repository via
// paginated /v2/<repo>/tags/list. Follows Link: rel="next" until
// exhausted. Uses the same bearer-token dance as HeadManifest.
func (c *Client) ListTags(ctx context.Context, ref Reference) ([]string, error) {
	if err := ref.validForHTTP(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, headManifestTimeout)
	defer cancel()

	url := fmt.Sprintf("https://%s/v2/%s/tags/list", ref.Registry, ref.Repository)
	var all []string
	var token string

	for pages := 0; url != "" && pages < 100; pages++ {
		page, next, err := c.getTagsPage(ctx, url, token)
		if err != nil {
			// If we got a challenge, resolve token once and retry the
			// same URL.
			var chal *bearerChallenge
			if errors.As(err, &chal) && c.auth != nil && token == "" {
				tk, tokErr := c.auth.AnonymousBearer(ctx, chal.challenge)
				if tokErr != nil {
					return nil, classifyErr("auth", fmt.Errorf("bearer token exchange: %w", tokErr))
				}
				token = tk
				continue // retry same URL with the token
			}
			return nil, err
		}
		all = append(all, page...)
		url = next
	}
	return all, nil
}

// getTagsPage fetches one tags-list page and returns the tag slice plus
// the absolute URL of the next page (empty if no Link: rel="next").
func (c *Client) getTagsPage(ctx context.Context, url, token string) ([]string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", classifyErr("parse", fmt.Errorf("build GET request: %w", err))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", classifyErr("network", fmt.Errorf("GET %s: %w", url, err))
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		body := io.LimitReader(resp.Body, maxManifestBody)
		var payload struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		if err := json.NewDecoder(body).Decode(&payload); err != nil {
			return nil, "", classifyErr("parse", fmt.Errorf("decode tags-list body: %w", err))
		}
		next := parseNextLink(resp.Header.Get("Link"), url)
		return payload.Tags, next, nil
	case http.StatusUnauthorized:
		challenge := resp.Header.Get("WWW-Authenticate")
		if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
			return nil, "", classifyErr("auth", fmt.Errorf("GET %s: 401 without Bearer challenge", url))
		}
		return nil, "", &bearerChallenge{challenge: challenge}
	case http.StatusNotFound:
		return nil, "", classifyErr("not_found", fmt.Errorf("GET %s: 404", url))
	default:
		return nil, "", classifyErr("network", fmt.Errorf("GET %s: unexpected status %d", url, resp.StatusCode))
	}
}

// parseNextLink extracts the rel="next" URL from a Link header. Returns
// "" when no next link is present. base is the current-page URL, used to
// resolve relative Link targets.
func parseNextLink(link, base string) string {
	if link == "" {
		return ""
	}
	// Link header format: `<url>; rel="next", <url2>; rel="prev"`
	for _, entry := range strings.Split(link, ",") {
		entry = strings.TrimSpace(entry)
		semi := strings.Index(entry, ";")
		if semi < 0 {
			continue
		}
		target := strings.TrimSpace(entry[:semi])
		params := strings.TrimSpace(entry[semi+1:])
		if !strings.Contains(strings.ToLower(params), `rel="next"`) {
			continue
		}
		target = strings.TrimPrefix(strings.TrimSuffix(target, ">"), "<")
		// Relative URL — resolve against base's scheme+host.
		if strings.HasPrefix(target, "/") {
			if slash := strings.Index(base, "://"); slash >= 0 {
				after := base[slash+3:]
				if hostEnd := strings.Index(after, "/"); hostEnd >= 0 {
					return base[:slash+3] + after[:hostEnd] + target
				}
				return base[:slash+3] + after + target
			}
		}
		return target
	}
	return ""
}

// bearerChallenge is an internal sentinel error signalling that the
// caller should perform the bearer-token dance. It carries the raw
// WWW-Authenticate header so auth.go can parse realm/service/scope.
type bearerChallenge struct {
	challenge string
}

func (b *bearerChallenge) Error() string {
	return "registry requires bearer token: " + b.challenge
}

// validForHTTP checks that a Reference has enough populated fields to
// make an HTTP request. Returns nil on success.
func (r Reference) validForHTTP() error {
	if r.Registry == "" {
		return classifyErr("parse", errors.New("empty registry in reference"))
	}
	if r.Repository == "" {
		return classifyErr("parse", errors.New("empty repository in reference"))
	}
	if r.Tag == "" && r.Digest == "" {
		return classifyErr("parse", errors.New("reference has neither tag nor digest"))
	}
	return nil
}

// LatestSemverTag returns the highest semver-shaped tag from tags that
// matches the given suffix family, or ("", false) if no tag qualifies.
//
// A tag qualifies iff it matches the shape "X.Y.Z" or "X.Y.Z<suffix>"
// where <suffix> starts with a non-digit character (e.g. "-slim",
// "-bookworm-slim"). family="" selects pure X.Y.Z tags; family="-slim"
// selects "X.Y.Z-slim" only. Tags with a non-matching suffix are
// ignored.
//
// Comparison is a lexicographic tuple over (major, minor, patch)
// parsed via strconv.Atoi — no dependency on golang.org/x/mod/semver;
// we don't need the full semver grammar, only the X.Y.Z prefix.
func LatestSemverTag(tags []string, family string) (string, bool) {
	var (
		bestTag         string
		bestMaj, bestMi int
		bestPa          int
		found           bool
	)
	for _, tag := range tags {
		maj, mi, pa, suffix, ok := parseSemverPrefix(tag)
		if !ok {
			continue
		}
		if suffix != family {
			continue
		}
		if !found ||
			maj > bestMaj ||
			(maj == bestMaj && mi > bestMi) ||
			(maj == bestMaj && mi == bestMi && pa > bestPa) {
			bestTag = tag
			bestMaj, bestMi, bestPa = maj, mi, pa
			found = true
		}
	}
	return bestTag, found
}

// parseSemverPrefix splits "X.Y.Z" or "X.Y.Z<suffix>" into
// (major, minor, patch, suffix, ok). ok is false when the input does
// not match this shape. suffix, if present, must start with a
// non-digit character (e.g. "-slim") — this rules out partial
// versions like "44.11.3.1" being misread.
func parseSemverPrefix(tag string) (int, int, int, string, bool) {
	// Take numeric-and-dot prefix, then split into three parts.
	end := 0
	dots := 0
	for end < len(tag) {
		ch := tag[end]
		if ch >= '0' && ch <= '9' {
			end++
			continue
		}
		if ch == '.' && dots < 2 {
			dots++
			end++
			continue
		}
		break
	}
	prefix := tag[:end]
	suffix := tag[end:]
	if dots != 2 {
		return 0, 0, 0, "", false
	}
	// Guard against tags like "1.2.": prefix ends with a "." with no
	// digit following.
	parts := strings.Split(prefix, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return 0, 0, 0, "", false
	}
	// Suffix must be empty OR start with a non-digit AND not with a
	// literal `.` (which would mean the input is `X.Y.Z.something`).
	if suffix != "" {
		if suffix[0] == '.' {
			return 0, 0, 0, "", false
		}
		if suffix[0] >= '0' && suffix[0] <= '9' {
			return 0, 0, 0, "", false
		}
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, "", false
	}
	mi, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, "", false
	}
	pa, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0, "", false
	}
	return maj, mi, pa, suffix, true
}
