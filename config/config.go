// Package config provides environment-based configuration for the renovate-docker-operator.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// envOrDefaultDuration parses a duration string env var or returns a default.
func envOrDefaultDuration(key string, defaultValue time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return defaultValue
	}
	return d
}

// Config holds all operator configuration loaded from environment variables.
type Config struct {
	// Database
	SQLitePath string // ROP_SQLITE_PATH (default: /data/renovate.db)

	// Docker
	RenovateImage            string // ROP_IMAGE (default: renovate/renovate:latest)
	CacheVolume              string // ROP_CACHE_VOLUME (default: renovate-cache)
	ContainerbaseCacheVolume string // ROP_CONTAINERBASE_CACHE_VOLUME (default: renovate-containerbase-cache)
	ContainerNetwork         string // ROP_CONTAINER_NETWORK (default: "")
	ImagePullPolicy          string // ROP_IMAGE_PULL_POLICY (default: if-not-present)

	// Execution
	Parallelism int           // ROP_PARALLELISM (default: 2)
	JobTimeout  time.Duration // ROP_JOB_TIMEOUT (default: 1800)
	GracePeriod time.Duration // ROP_SHUTDOWN_GRACE_PERIOD (default: 300)

	// Platform
	Platform         string // RENOVATE_PLATFORM (default: forgejo)
	PlatformEndpoint string // ROP_PLATFORM_ENDPOINT (required)
	PlatformToken    string // RENOVATE_TOKEN (required)

	// Schedule
	CronSchedule      string // ROP_CRON_SCHEDULE (default: 0 */4 * * *)
	CronSkipDiscovery bool   // ROP_CRON_SKIP_DISCOVERY (default: false)

	// Server
	ServerPort     string // ROP_SERVER_PORT (default: 8081)
	WebhookEnabled bool   // ROP_WEBHOOK_ENABLED (default: true)

	// Auth
	OIDCIssuerURL    string // ROP_OIDC_ISSUER_URL (optional)
	OIDCClientID     string // ROP_OIDC_CLIENT_ID (optional)
	OIDCClientSecret string // ROP_OIDC_CLIENT_SECRET (optional)
	OIDCRedirectURL  string // ROP_OIDC_REDIRECT_URL (optional)
	SessionSecret    string // ROP_SESSION_SECRET (optional, auto-generated if empty)

	// Discovery
	DiscoveryFilters string // ROP_DISCOVERY_FILTERS (comma-sep, optional)
	DiscoverTopics   string // ROP_DISCOVER_TOPICS (comma-sep, optional)
	SkipForks        bool   // ROP_SKIP_FORKS (default: false)

	// Logging
	LogLevel string // ROP_LOG_LEVEL (default: info)

	// Security
	MaxRequestBody int64 // ROP_MAX_REQUEST_BODY (default: 2 MiB)

	// Image Cache
	ImageCacheTTL time.Duration // ROP_IMAGE_CACHE_TTL (default: 24h, 0 disables)

	// Image update poller
	CheckUpdateRunnerImage  bool          // ROP_CHECK_UPDATE_RUNNER_IMAGE (default: true)
	ImageUpdatePollInterval time.Duration // ROP_IMAGE_UPDATE_POLL_INTERVAL (default: value of ImageCacheTTL)

	// Resilience
	RapidFailThreshold int           // ROP_RAPID_FAIL_THRESHOLD (default: 10)
	RapidFailWindow    time.Duration // ROP_RAPID_FAIL_WINDOW (default: 5m)
	FailureMinRuntime  time.Duration // ROP_FAILURE_MIN_RUNTIME (default: 30s)
	BackoffBase        time.Duration // ROP_BACKOFF_BASE (default: 1m)
	BackoffMax         time.Duration // ROP_BACKOFF_MAX (default: 1h)
	ReplayQueueCap     int           // ROP_REPLAY_QUEUE_CAP (default: 10000)

	// Metrics
	MetricsProjectLabel string // ROP_METRICS_PROJECT_LABEL (default: all) — all/breaker/off
}

// configValues stores a flat map of config values for GetValue lookups.
// Safety: Load() is called exactly once in main() before any goroutines start,
// so concurrent read access via GetValue() is safe without additional synchronization.
var configValues map[string]string

// Load reads configuration from environment variables and returns a Config struct.
// Returns an error if required variables are missing.
func Load() (*Config, error) {
	cfg := &Config{
		SQLitePath:               envOrDefault("ROP_SQLITE_PATH", "/data/renovate.db"),
		RenovateImage:            envOrDefault("ROP_IMAGE", "renovate/renovate:latest"),
		CacheVolume:              envOrDefault("ROP_CACHE_VOLUME", "renovate-cache"),
		ContainerbaseCacheVolume: envOrDefault("ROP_CONTAINERBASE_CACHE_VOLUME", "renovate-containerbase-cache"),
		ContainerNetwork:         os.Getenv("ROP_CONTAINER_NETWORK"),
		ImagePullPolicy:          envOrDefault("ROP_IMAGE_PULL_POLICY", "if-not-present"),
		Platform:                 envOrDefault("RENOVATE_PLATFORM", "forgejo"),
		PlatformEndpoint:         os.Getenv("ROP_PLATFORM_ENDPOINT"),
		PlatformToken:            os.Getenv("RENOVATE_TOKEN"),
		CronSchedule:             envOrDefault("ROP_CRON_SCHEDULE", "0 */4 * * *"),
		ServerPort:               envOrDefault("ROP_SERVER_PORT", "8081"),
		OIDCIssuerURL:            os.Getenv("ROP_OIDC_ISSUER_URL"),
		OIDCClientID:             os.Getenv("ROP_OIDC_CLIENT_ID"),
		OIDCClientSecret:         os.Getenv("ROP_OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:          os.Getenv("ROP_OIDC_REDIRECT_URL"),
		SessionSecret:            os.Getenv("ROP_SESSION_SECRET"),
		DiscoveryFilters:         os.Getenv("ROP_DISCOVERY_FILTERS"),
		DiscoverTopics:           os.Getenv("ROP_DISCOVER_TOPICS"),
		LogLevel:                 envOrDefault("ROP_LOG_LEVEL", "info"),
	}

	// Parse integers
	cfg.Parallelism = envOrDefaultInt("ROP_PARALLELISM", 2)

	// Parse durations from seconds
	jobTimeoutSec := envOrDefaultInt("ROP_JOB_TIMEOUT", 1800)
	cfg.JobTimeout = time.Duration(jobTimeoutSec) * time.Second

	gracePeriodSec := envOrDefaultInt("ROP_SHUTDOWN_GRACE_PERIOD", 300)
	cfg.GracePeriod = time.Duration(gracePeriodSec) * time.Second

	// Parse booleans
	cfg.CronSkipDiscovery = envOrDefaultBool("ROP_CRON_SKIP_DISCOVERY", false)
	cfg.WebhookEnabled = envOrDefaultBool("ROP_WEBHOOK_ENABLED", true)
	cfg.SkipForks = envOrDefaultBool("ROP_SKIP_FORKS", false)

	// Auto-generate session secret if not provided
	if cfg.SessionSecret == "" {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("failed to generate session secret: %w", err)
		}
		cfg.SessionSecret = hex.EncodeToString(secret)
	}

	// Parse security limits
	cfg.MaxRequestBody = envOrDefaultInt64("ROP_MAX_REQUEST_BODY", 2*1024*1024)

	// Parse image cache TTL
	cfg.ImageCacheTTL = envOrDefaultDuration("ROP_IMAGE_CACHE_TTL", 24*time.Hour)

	// Parse image update poller settings.
	// ROP_IMAGE_UPDATE_POLL_INTERVAL defaults to the value of ImageCacheTTL
	// so operators who tune the executor's re-pull cadence get a matching
	// poll cadence for free (documented interaction).
	cfg.CheckUpdateRunnerImage = envOrDefaultBool("ROP_CHECK_UPDATE_RUNNER_IMAGE", true)
	cfg.ImageUpdatePollInterval = envOrDefaultDuration("ROP_IMAGE_UPDATE_POLL_INTERVAL", cfg.ImageCacheTTL)

	// Parse resilience settings
	cfg.RapidFailThreshold = envOrDefaultInt("ROP_RAPID_FAIL_THRESHOLD", 10)
	cfg.RapidFailWindow = envOrDefaultDuration("ROP_RAPID_FAIL_WINDOW", 5*time.Minute)
	cfg.FailureMinRuntime = envOrDefaultDuration("ROP_FAILURE_MIN_RUNTIME", 30*time.Second)
	cfg.BackoffBase = envOrDefaultDuration("ROP_BACKOFF_BASE", 1*time.Minute)
	cfg.BackoffMax = envOrDefaultDuration("ROP_BACKOFF_MAX", 1*time.Hour)
	cfg.ReplayQueueCap = envOrDefaultInt("ROP_REPLAY_QUEUE_CAP", 10000)

	// Parse metrics settings
	cfg.MetricsProjectLabel = envOrDefault("ROP_METRICS_PROJECT_LABEL", "all")

	// Validate required fields
	if cfg.PlatformEndpoint == "" {
		return nil, fmt.Errorf("ROP_PLATFORM_ENDPOINT is required but not set")
	}

	if cfg.PlatformToken == "" {
		return nil, fmt.Errorf("RENOVATE_TOKEN environment variable is required but not set")
	}

	// Populate the flat map for GetValue
	configValues = map[string]string{
		"ROP_SQLITE_PATH":                cfg.SQLitePath,
		"ROP_IMAGE":                      cfg.RenovateImage,
		"ROP_CACHE_VOLUME":               cfg.CacheVolume,
		"ROP_CONTAINERBASE_CACHE_VOLUME": cfg.ContainerbaseCacheVolume,
		"ROP_CONTAINER_NETWORK":          cfg.ContainerNetwork,
		"ROP_IMAGE_PULL_POLICY":          cfg.ImagePullPolicy,
		"ROP_PARALLELISM":                strconv.Itoa(cfg.Parallelism),
		"ROP_JOB_TIMEOUT":                strconv.Itoa(jobTimeoutSec),
		"ROP_SHUTDOWN_GRACE_PERIOD":      strconv.Itoa(gracePeriodSec),
		"RENOVATE_PLATFORM":              cfg.Platform,
		"ROP_PLATFORM_ENDPOINT":          cfg.PlatformEndpoint,
		"RENOVATE_TOKEN":                 cfg.PlatformToken,
		"ROP_CRON_SCHEDULE":              cfg.CronSchedule,
		"ROP_CRON_SKIP_DISCOVERY":        strconv.FormatBool(cfg.CronSkipDiscovery),
		"ROP_SERVER_PORT":                cfg.ServerPort,
		"ROP_WEBHOOK_ENABLED":            strconv.FormatBool(cfg.WebhookEnabled),
		"ROP_OIDC_ISSUER_URL":            cfg.OIDCIssuerURL,
		"ROP_OIDC_CLIENT_ID":             cfg.OIDCClientID,
		"ROP_OIDC_CLIENT_SECRET":         cfg.OIDCClientSecret,
		"ROP_OIDC_REDIRECT_URL":          cfg.OIDCRedirectURL,
		"ROP_SESSION_SECRET":             cfg.SessionSecret,
		"ROP_DISCOVERY_FILTERS":          cfg.DiscoveryFilters,
		"ROP_DISCOVER_TOPICS":            cfg.DiscoverTopics,
		"ROP_SKIP_FORKS":                 strconv.FormatBool(cfg.SkipForks),
		"ROP_LOG_LEVEL":                  cfg.LogLevel,
		"ROP_MAX_REQUEST_BODY":           strconv.FormatInt(cfg.MaxRequestBody, 10),
		"ROP_IMAGE_CACHE_TTL":            cfg.ImageCacheTTL.String(),
		"ROP_CHECK_UPDATE_RUNNER_IMAGE":  strconv.FormatBool(cfg.CheckUpdateRunnerImage),
		"ROP_IMAGE_UPDATE_POLL_INTERVAL": cfg.ImageUpdatePollInterval.String(),
		"ROP_RAPID_FAIL_THRESHOLD":       strconv.Itoa(cfg.RapidFailThreshold),
		"ROP_RAPID_FAIL_WINDOW":          cfg.RapidFailWindow.String(),
		"ROP_FAILURE_MIN_RUNTIME":        cfg.FailureMinRuntime.String(),
		"ROP_BACKOFF_BASE":               cfg.BackoffBase.String(),
		"ROP_BACKOFF_MAX":                cfg.BackoffMax.String(),
		"ROP_REPLAY_QUEUE_CAP":           strconv.Itoa(cfg.ReplayQueueCap),
		"ROP_METRICS_PROJECT_LABEL":      cfg.MetricsProjectLabel,
	}

	return cfg, nil
}

// GetValue provides backward-compatible access to config by key name.
// Returns empty string if key is not found or config has not been loaded.
func GetValue(key string) string {
	if configValues == nil {
		return ""
	}
	return configValues[key]
}

// envOrDefault returns the environment variable value or a default.
func envOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// envOrDefaultInt parses an integer env var or returns a default.
func envOrDefaultInt(key string, defaultValue int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	i, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return defaultValue
	}
	return i
}

// envOrDefaultInt64 parses an int64 env var or returns a default.
func envOrDefaultInt64(key string, defaultValue int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	i, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return defaultValue
	}
	return i
}

// envOrDefaultBool parses a boolean env var or returns a default.
func envOrDefaultBool(key string, defaultValue bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return defaultValue
	}
	return b
}

// MinImageUpdatePollInterval is the floor for ROP_IMAGE_UPDATE_POLL_INTERVAL
// enforced by Validate when the poller is enabled. 0 remains valid and
// means "run once at startup only, no ticker." See plan
// 2026-08-04-renovate-image-freshness-and-update-notification-plan.md §D.
const MinImageUpdatePollInterval = 15 * time.Minute

// Validate performs boot-time validation of a loaded Config that goes
// beyond individual env-var parsing. Called from cmd/main.go
// immediately after Load(); non-nil error is fatal.
//
// parseRef is the reference parser used when the poller is enabled
// (typically imageupdate.Parse). Passed as a function so this package
// does not import internal/imageupdate directly. Nil is accepted and
// skips the ROP_IMAGE parseability check — useful for tests that
// don't want to depend on the imageupdate package.
func Validate(cfg *Config, parseRef func(string) error) error {
	if cfg == nil {
		return fmt.Errorf("nil config")
	}
	if !cfg.CheckUpdateRunnerImage {
		return nil // poller disabled: nothing to validate
	}
	if parseRef != nil {
		if err := parseRef(cfg.RenovateImage); err != nil {
			return fmt.Errorf("invalid ROP_IMAGE %q: %w", cfg.RenovateImage, err)
		}
	}
	// 0 is valid ("run once at startup, no ticker"). Any positive value
	// below the floor is a fat-finger config.
	if cfg.ImageUpdatePollInterval > 0 && cfg.ImageUpdatePollInterval < MinImageUpdatePollInterval {
		return fmt.Errorf(
			"ROP_IMAGE_UPDATE_POLL_INTERVAL=%s is below the %s minimum floor; "+
				"raise it or disable the poller with ROP_CHECK_UPDATE_RUNNER_IMAGE=false",
			cfg.ImageUpdatePollInterval,
			MinImageUpdatePollInterval,
		)
	}
	return nil
}
