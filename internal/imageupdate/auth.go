package imageupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Authenticator resolves a WWW-Authenticate: Bearer challenge into an
// anonymous access token. v1 only ships one implementation
// (AnonymousAuth); a future auth-source plan can add credentialed
// implementations without touching the Client.
type Authenticator interface {
	AnonymousBearer(ctx context.Context, challenge string) (token string, err error)
}

// AnonymousAuth is the v1 Authenticator. It parses Bearer challenges
// and fetches anonymous tokens against whatever realm the registry
// declares. No static credentials, no credHelper, no env var.
type AnonymousAuth struct {
	http *http.Client
	// userAgent is sent on every realm request.
	userAgent string

	mu    sync.Mutex
	cache map[string]cachedToken
}

// cachedToken is an in-memory per-(realm, service, scope) token cache
// entry. TTL is short (60s) — most registries hand out 300s tokens, so
// this is aggressively conservative but keeps invalidation simple.
type cachedToken struct {
	token   string
	expires time.Time
}

const tokenCacheTTL = 60 * time.Second

// NewAnonymousAuth constructs an AnonymousAuth sharing the operator's
// User-Agent string. The provided *http.Client should be the same one
// the registry client uses so ResponseHeaderTimeout and redirect
// policy apply uniformly.
func NewAnonymousAuth(httpClient *http.Client, userAgent string) *AnonymousAuth {
	return &AnonymousAuth{
		http:      httpClient,
		userAgent: userAgent,
		cache:     make(map[string]cachedToken),
	}
}

// AnonymousBearer parses a WWW-Authenticate: Bearer challenge, GETs
// the realm anonymously, and returns the resulting access token.
// Prefers the "token" JSON field; falls back to "access_token" (some
// registries use one, some the other).
//
// Cache: hits on the (realm, service, scope) triple within 60s return
// the cached token. Cache is invalidated on next tick if the caller
// re-invokes AnonymousBearer with the same challenge (e.g. after a
// 401 with a stale token).
func (a *AnonymousAuth) AnonymousBearer(ctx context.Context, challenge string) (string, error) {
	params, err := parseChallenge(challenge)
	if err != nil {
		return "", err
	}
	cacheKey := params["realm"] + "|" + params["service"] + "|" + params["scope"]

	a.mu.Lock()
	if entry, ok := a.cache[cacheKey]; ok && time.Now().Before(entry.expires) {
		a.mu.Unlock()
		return entry.token, nil
	}
	a.mu.Unlock()

	token, err := a.fetchToken(ctx, params)
	if err != nil {
		return "", err
	}

	a.mu.Lock()
	a.cache[cacheKey] = cachedToken{
		token:   token,
		expires: time.Now().Add(tokenCacheTTL),
	}
	a.mu.Unlock()
	return token, nil
}

// fetchToken performs the realm GET. Uses tokenRealmTimeout as the per-
// call context timeout, on top of any caller-supplied deadline.
func (a *AnonymousAuth) fetchToken(ctx context.Context, params map[string]string) (string, error) {
	realm, ok := params["realm"]
	if !ok || realm == "" {
		return "", errors.New("bearer challenge missing realm")
	}
	// Build realm URL with service+scope as query params.
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse realm URL %q: %w", realm, err)
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	if scope := params["scope"]; scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, tokenRealmTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build realm request: %w", err)
	}
	req.Header.Set("User-Agent", a.userAgent)
	req.Header.Set("Accept", "application/json")
	// Anonymous only in v1: no Authorization header on the realm call.

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("realm GET %s: %w", u.String(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("realm GET %s: status %d", u.String(), resp.StatusCode)
	}

	body := io.LimitReader(resp.Body, maxManifestBody)
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode realm response: %w", err)
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	if payload.AccessToken != "" {
		return payload.AccessToken, nil
	}
	return "", errors.New("realm response missing token and access_token fields")
}

// parseChallenge parses a WWW-Authenticate: Bearer header into its
// component parameters (realm, service, scope, and any others).
// Accepts the challenge string with or without the leading "Bearer "
// prefix. Unquotes values enclosed in double quotes.
func parseChallenge(challenge string) (map[string]string, error) {
	s := strings.TrimSpace(challenge)
	// Strip a leading "Bearer " (case-insensitive).
	if len(s) >= 7 && strings.EqualFold(s[:7], "Bearer ") {
		s = s[7:]
	}
	if s == "" {
		return nil, errors.New("empty bearer challenge")
	}
	out := make(map[string]string)
	// Split on commas that are not inside quotes.
	fields := splitOutsideQuotes(s, ',')
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		eq := strings.Index(field, "=")
		if eq < 0 {
			// Bare token — ignore.
			continue
		}
		key := strings.TrimSpace(field[:eq])
		value := strings.TrimSpace(field[eq+1:])
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		out[strings.ToLower(key)] = value
	}
	if _, ok := out["realm"]; !ok {
		return nil, errors.New("bearer challenge missing realm parameter")
	}
	return out, nil
}

// splitOutsideQuotes splits s on sep, but only when sep is not inside a
// double-quoted substring. Used by parseChallenge because
// WWW-Authenticate values may legally contain commas inside quotes
// (e.g. scope="repository:foo,bar:pull").
func splitOutsideQuotes(s string, sep byte) []string {
	var out []string
	start := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case sep:
			if !inQuote {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}
