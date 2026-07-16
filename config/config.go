// Package config provides environment-based configuration for the renovate-docker-operator.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all operator configuration loaded from environment variables.
type Config struct {
	// Database
	SQLitePath string // SQLITE_PATH (default: /data/renovate.db)

	// Docker
	RenovateImage    string // RENOVATE_IMAGE (default: renovate/renovate:latest)
	CacheVolume      string // CACHE_VOLUME (default: renovate-cache)
	ContainerNetwork string // CONTAINER_NETWORK (default: "")
	ImagePullPolicy  string // IMAGE_PULL_POLICY (default: if-not-present)

	// Execution
	Parallelism int           // GLOBAL_PARALLELISM_LIMIT (default: 2)
	JobTimeout  time.Duration // JOB_TIMEOUT_SECONDS (default: 1800)
	GracePeriod time.Duration // SHUTDOWN_GRACE_PERIOD (default: 300)

	// Platform
	Platform         string // PLATFORM (default: forgejo)
	PlatformEndpoint string // PLATFORM_ENDPOINT (required)
	PlatformToken    string // RENOVATE_TOKEN (required)

	// Schedule
	CronSchedule      string // CRON_SCHEDULE (default: 0 */4 * * *)
	CronSkipDiscovery bool   // CRON_SKIP_DISCOVERY (default: false)

	// Server
	ServerPort     string // SERVER_PORT (default: 8081)
	WebhookEnabled bool   // WEBHOOK_SERVER_ENABLED (default: true)
	WebhookSecret  string // WEBHOOK_SECRET (optional, comma-separated)

	// Auth
	OIDCIssuerURL    string // OIDC_ISSUER_URL (optional)
	OIDCClientID     string // OIDC_CLIENT_ID (optional)
	OIDCClientSecret string // OIDC_CLIENT_SECRET (optional)
	OIDCRedirectURL  string // OIDC_REDIRECT_URL (optional)
	SessionSecret    string // SESSION_SECRET (optional, auto-generated if empty)

	// Discovery
	DiscoveryFilters string // RENOVATE_DISCOVERY_FILTERS (comma-sep, optional)
	DiscoverTopics   string // RENOVATE_DISCOVER_TOPICS (comma-sep, optional)
	SkipForks        bool   // AUTODISCOVER_SKIP_FORKS (default: false)

	// Logging
	LogLevel string // LOG_LEVEL (default: info)
}

// configValues stores a flat map of config values for GetValue lookups.
var configValues map[string]string

// Load reads configuration from environment variables and returns a Config struct.
// Returns an error if required variables are missing.
func Load() (*Config, error) {
	cfg := &Config{
		SQLitePath:       envOrDefault("SQLITE_PATH", "/data/renovate.db"),
		RenovateImage:    envOrDefault("RENOVATE_IMAGE", "renovate/renovate:latest"),
		CacheVolume:      envOrDefault("CACHE_VOLUME", "renovate-cache"),
		ContainerNetwork: os.Getenv("CONTAINER_NETWORK"),
		ImagePullPolicy:  envOrDefault("IMAGE_PULL_POLICY", "if-not-present"),
		Platform:         envOrDefault("PLATFORM", "forgejo"),
		PlatformEndpoint: os.Getenv("PLATFORM_ENDPOINT"),
		PlatformToken:    os.Getenv("RENOVATE_TOKEN"),
		CronSchedule:     envOrDefault("CRON_SCHEDULE", "0 */4 * * *"),
		ServerPort:       envOrDefault("SERVER_PORT", "8081"),
		WebhookSecret:    os.Getenv("WEBHOOK_SECRET"),
		OIDCIssuerURL:    os.Getenv("OIDC_ISSUER_URL"),
		OIDCClientID:     os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:  os.Getenv("OIDC_REDIRECT_URL"),
		SessionSecret:    os.Getenv("SESSION_SECRET"),
		DiscoveryFilters: os.Getenv("RENOVATE_DISCOVERY_FILTERS"),
		DiscoverTopics:   os.Getenv("RENOVATE_DISCOVER_TOPICS"),
		LogLevel:         envOrDefault("LOG_LEVEL", "info"),
	}

	// Parse integers
	cfg.Parallelism = envOrDefaultInt("GLOBAL_PARALLELISM_LIMIT", 2)

	// Parse durations from seconds
	jobTimeoutSec := envOrDefaultInt("JOB_TIMEOUT_SECONDS", 1800)
	cfg.JobTimeout = time.Duration(jobTimeoutSec) * time.Second

	gracePeriodSec := envOrDefaultInt("SHUTDOWN_GRACE_PERIOD", 300)
	cfg.GracePeriod = time.Duration(gracePeriodSec) * time.Second

	// Parse booleans
	cfg.CronSkipDiscovery = envOrDefaultBool("CRON_SKIP_DISCOVERY", false)
	cfg.WebhookEnabled = envOrDefaultBool("WEBHOOK_SERVER_ENABLED", true)
	cfg.SkipForks = envOrDefaultBool("AUTODISCOVER_SKIP_FORKS", false)

	// Validate required fields
	if cfg.PlatformEndpoint == "" {
		return nil, fmt.Errorf("PLATFORM_ENDPOINT is required but not set")
	}

	// Populate the flat map for GetValue
	configValues = map[string]string{
		"SQLITE_PATH":                cfg.SQLitePath,
		"RENOVATE_IMAGE":             cfg.RenovateImage,
		"CACHE_VOLUME":               cfg.CacheVolume,
		"CONTAINER_NETWORK":          cfg.ContainerNetwork,
		"IMAGE_PULL_POLICY":          cfg.ImagePullPolicy,
		"GLOBAL_PARALLELISM_LIMIT":   strconv.Itoa(cfg.Parallelism),
		"JOB_TIMEOUT_SECONDS":        strconv.Itoa(jobTimeoutSec),
		"SHUTDOWN_GRACE_PERIOD":      strconv.Itoa(gracePeriodSec),
		"PLATFORM":                   cfg.Platform,
		"PLATFORM_ENDPOINT":          cfg.PlatformEndpoint,
		"RENOVATE_TOKEN":             cfg.PlatformToken,
		"CRON_SCHEDULE":              cfg.CronSchedule,
		"CRON_SKIP_DISCOVERY":        strconv.FormatBool(cfg.CronSkipDiscovery),
		"SERVER_PORT":                cfg.ServerPort,
		"WEBHOOK_SERVER_ENABLED":     strconv.FormatBool(cfg.WebhookEnabled),
		"WEBHOOK_SECRET":             cfg.WebhookSecret,
		"OIDC_ISSUER_URL":            cfg.OIDCIssuerURL,
		"OIDC_CLIENT_ID":             cfg.OIDCClientID,
		"OIDC_CLIENT_SECRET":         cfg.OIDCClientSecret,
		"OIDC_REDIRECT_URL":          cfg.OIDCRedirectURL,
		"SESSION_SECRET":             cfg.SessionSecret,
		"RENOVATE_DISCOVERY_FILTERS": cfg.DiscoveryFilters,
		"RENOVATE_DISCOVER_TOPICS":   cfg.DiscoverTopics,
		"AUTODISCOVER_SKIP_FORKS":    strconv.FormatBool(cfg.SkipForks),
		"LOG_LEVEL":                  cfg.LogLevel,
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
