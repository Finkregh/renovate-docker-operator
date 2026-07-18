package executor

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name:   "slash to dash",
			input:  "org/repo",
			expect: "org-repo",
		},
		{
			name:   "dot to dash",
			input:  "my.project",
			expect: "my-project",
		},
		{
			name:   "colon to dash",
			input:  "registry:tag",
			expect: "registry-tag",
		},
		{
			name:   "space to dash",
			input:  "my project",
			expect: "my-project",
		},
		{
			name:   "uppercased input is lowered",
			input:  "MyOrg/MyRepo",
			expect: "myorg-myrepo",
		},
		{
			name:   "mixed special chars",
			input:  "org/repo.name:v1 tag",
			expect: "org-repo-name-v1-tag",
		},
		{
			name:   "truncates to 60 chars",
			input:  strings.Repeat("a", 100),
			expect: strings.Repeat("a", 60),
		},
		{
			name:   "exactly 60 chars not truncated",
			input:  strings.Repeat("b", 60),
			expect: strings.Repeat("b", 60),
		},
		{
			name:   "empty string",
			input:  "",
			expect: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeName(tc.input)
			if got != tc.expect {
				t.Errorf("sanitizeName(%q) = %q, want %q", tc.input, got, tc.expect)
			}
		})
	}
}

func TestIsDockerMultiplexed(t *testing.T) {
	tests := []struct {
		name   string
		data   []byte
		expect bool
	}{
		{
			name: "valid stdout header",
			// stream type=1 (stdout), padding=0x00 x3, payload len=5 (big-endian), then 5 bytes payload
			data:   []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'},
			expect: true,
		},
		{
			name: "valid stderr header",
			// stream type=2 (stderr), payload len=3
			data:   []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 'e', 'r', 'r'},
			expect: true,
		},
		{
			name:   "raw text not multiplexed",
			data:   []byte(`{"level":30,"msg":"hello"}`),
			expect: false,
		},
		{
			name:   "too short",
			data:   []byte{0x01, 0x00, 0x00},
			expect: false,
		},
		{
			name: "wrong padding byte",
			// padding bytes should be 0x00, but byte 1 is 0x01
			data:   []byte{0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'},
			expect: false,
		},
		{
			name: "zero payload length",
			// stream type=1, valid padding, but payload length = 0
			data:   []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			expect: false,
		},
		{
			name: "payload length exceeds remaining data",
			// Claims 100 bytes but only has 3 after header
			data:   []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x64, 'a', 'b', 'c'},
			expect: false,
		},
		{
			name: "invalid stream type",
			// stream type=3 is invalid (only 1 and 2 are valid)
			data:   []byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 'x'},
			expect: false,
		},
		{
			name:   "empty data",
			data:   []byte{},
			expect: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isDockerMultiplexed(tc.data)
			if got != tc.expect {
				t.Errorf("isDockerMultiplexed(%v) = %v, want %v", tc.data, got, tc.expect)
			}
		})
	}
}

// newTestExecutor creates a DockerExecutor for white-box testing without a Docker daemon.
func newTestExecutor(t *testing.T) *DockerExecutor {
	t.Helper()
	return &DockerExecutor{
		logger:     slog.Default(),
		running:    make(map[string]string),
		imageCache: make(map[string]time.Time),
	}
}

func TestBuildEnvVars_Basic(t *testing.T) {
	e := newTestExecutor(t)

	// Clear env vars that would interfere
	t.Setenv("RENOVATEOP_TOKEN", "")
	t.Setenv("LOG_LEVEL", "")

	job := &api.RenovateJob{
		Name: "test-job",
		Spec: api.RenovateJobSpec{
			Provider: &api.RenovateProvider{
				Name:     "forgejo",
				Endpoint: "https://git.example.com",
			},
		},
	}

	result := e.buildEnvVars(job, false)

	// Convert to map for easier lookup
	envMap := envSliceToMap(result)

	// Check defaults
	assertEnv(t, envMap, "RENOVATE_LOG_FORMAT", "json")
	assertEnv(t, envMap, "NODE_NO_WARNINGS", "1")
	assertEnv(t, envMap, "RENOVATE_BASE_DIR", "/tmp/renovate")
	assertEnv(t, envMap, "LOG_LEVEL", "debug")

	// Check provider env vars
	assertEnv(t, envMap, "RENOVATE_PLATFORM", "forgejo")
	assertEnv(t, envMap, "RENOVATE_ENDPOINT", "https://git.example.com")

	// Verify output is sorted
	if !slices.IsSorted(result) {
		t.Errorf("result is not sorted: %v", result)
	}
}

func TestBuildEnvVars_Discovery(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")

	job := &api.RenovateJob{
		Name: "disco-job",
		Spec: api.RenovateJobSpec{
			Provider: &api.RenovateProvider{
				Name:     "forgejo",
				Endpoint: "https://git.example.com",
			},
			DiscoveryFilters: []string{"org/repo1", "org/repo2"},
			DiscoverTopics:   []string{"renovate", "managed"},
		},
	}

	result := e.buildEnvVars(job, true)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "RENOVATE_AUTODISCOVER_FILTER", "org/repo1,org/repo2")
	assertEnv(t, envMap, "RENOVATE_AUTODISCOVER_TOPICS", "renovate,managed")
}

func TestBuildEnvVars_DiscoveryVarsNotSetForNonDiscovery(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")

	job := &api.RenovateJob{
		Name: "non-disco-job",
		Spec: api.RenovateJobSpec{
			DiscoveryFilters: []string{"org/repo1"},
			DiscoverTopics:   []string{"renovate"},
		},
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	if _, ok := envMap["RENOVATE_AUTODISCOVER_FILTER"]; ok {
		t.Error("RENOVATE_AUTODISCOVER_FILTER should not be set for non-discovery runs")
	}
	if _, ok := envMap["RENOVATE_AUTODISCOVER_TOPICS"]; ok {
		t.Error("RENOVATE_AUTODISCOVER_TOPICS should not be set for non-discovery runs")
	}
}

func TestBuildEnvVars_ExtraEnvOverrides(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")

	job := &api.RenovateJob{
		Name: "override-job",
		Spec: api.RenovateJobSpec{
			ExtraEnv: []api.EnvVar{
				{Name: "RENOVATE_LOG_FORMAT", Value: "text"},
				{Name: "CUSTOM_VAR", Value: "custom-value"},
			},
		},
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	// ExtraEnv should override the default
	assertEnv(t, envMap, "RENOVATE_LOG_FORMAT", "text")
	// Custom var should be present
	assertEnv(t, envMap, "CUSTOM_VAR", "custom-value")
}

func TestBuildEnvVars_DebugModeTrue(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")
	t.Setenv("LOG_LEVEL", "")

	job := &api.RenovateJob{
		Name: "debug-job",
		Status: api.RenovateJobStatus{
			ExecutionOptions: &api.RenovateExecutionOptions{
				Debug: true,
			},
		},
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "LOG_LEVEL", "debug")
}

func TestBuildEnvVars_DefaultLogLevelDebug(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")
	t.Setenv("LOG_LEVEL", "")

	// No ExecutionOptions set at all — should default to debug
	job := &api.RenovateJob{
		Name: "no-options-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "LOG_LEVEL", "debug")
}

func TestBuildEnvVars_DebugModeFalse(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")
	t.Setenv("LOG_LEVEL", "")

	// User explicitly disabled debug mode
	job := &api.RenovateJob{
		Name: "no-debug-job",
		Status: api.RenovateJobStatus{
			ExecutionOptions: &api.RenovateExecutionOptions{
				Debug: false,
			},
		},
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "LOG_LEVEL", "info")
}

func TestBuildEnvVars_LogLevelEnvOverride(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")
	t.Setenv("LOG_LEVEL", "warn")

	// Env override should win over default debug
	job := &api.RenovateJob{
		Name: "env-override-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "LOG_LEVEL", "warn")
}

func TestBuildEnvVars_LogLevelEnvOverrideBeatsDebugFalse(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")
	t.Setenv("LOG_LEVEL", "debug")

	// Even when DB says debug=false, env override wins
	job := &api.RenovateJob{
		Name: "env-wins-job",
		Status: api.RenovateJobStatus{
			ExecutionOptions: &api.RenovateExecutionOptions{
				Debug: false,
			},
		},
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "LOG_LEVEL", "debug")
}

func TestBuildEnvVars_TokenPassthrough(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "my-secret-token")

	job := &api.RenovateJob{
		Name: "token-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "RENOVATE_TOKEN", "my-secret-token")
}

func TestBuildEnvVars_RenovateEnvPassthrough(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")
	t.Setenv("RENOVATE_CUSTOM_SETTING", "passed-through")

	job := &api.RenovateJob{
		Name: "passthrough-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "RENOVATE_CUSTOM_SETTING", "passed-through")
}

func TestBuildEnvVars_RenovateEnvPassthroughDoesNotOverrideExplicit(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATEOP_TOKEN", "")
	// Set an env var that would conflict with a predefined default
	t.Setenv("RENOVATE_LOG_FORMAT", "text-from-env")

	job := &api.RenovateJob{
		Name: "no-override-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	// The predefined default should win over passthrough
	assertEnv(t, envMap, "RENOVATE_LOG_FORMAT", "json")
}

func TestNew_Defaults(t *testing.T) {
	exec, err := New(Config{}, nil, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if exec.image != "renovate/renovate:latest" {
		t.Errorf("image = %q, want %q", exec.image, "renovate/renovate:latest")
	}
	if exec.network != "bridge" {
		t.Errorf("network = %q, want %q", exec.network, "bridge")
	}
	if exec.cacheVolume != "renovate-cache" {
		t.Errorf("cacheVolume = %q, want %q", exec.cacheVolume, "renovate-cache")
	}
	if exec.parallelism != 2 {
		t.Errorf("parallelism = %d, want %d", exec.parallelism, 2)
	}
	if exec.jobTimeout != 1800*time.Second {
		t.Errorf("jobTimeout = %v, want %v", exec.jobTimeout, 1800*time.Second)
	}
	if exec.gracePeriod != 300*time.Second {
		t.Errorf("gracePeriod = %v, want %v", exec.gracePeriod, 300*time.Second)
	}
	if exec.imagePullPolicy != "if-not-present" {
		t.Errorf("imagePullPolicy = %q, want %q", exec.imagePullPolicy, "if-not-present")
	}

	// Cleanup
	_ = exec.docker.Close()
}

func TestNew_CustomConfig(t *testing.T) {
	cfg := Config{
		Image:           "custom/image:v1",
		Network:         "host",
		CacheVolume:     "my-vol",
		Parallelism:     8,
		JobTimeout:      10 * time.Minute,
		GracePeriod:     30 * time.Second,
		ImagePullPolicy: "always",
		ImageCacheTTL:   5 * time.Minute,
	}

	exec, err := New(cfg, nil, slog.Default())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if exec.image != "custom/image:v1" {
		t.Errorf("image = %q, want %q", exec.image, "custom/image:v1")
	}
	if exec.network != "host" {
		t.Errorf("network = %q, want %q", exec.network, "host")
	}
	if exec.cacheVolume != "my-vol" {
		t.Errorf("cacheVolume = %q, want %q", exec.cacheVolume, "my-vol")
	}
	if exec.parallelism != 8 {
		t.Errorf("parallelism = %d, want %d", exec.parallelism, 8)
	}
	if exec.jobTimeout != 10*time.Minute {
		t.Errorf("jobTimeout = %v, want %v", exec.jobTimeout, 10*time.Minute)
	}
	if exec.gracePeriod != 30*time.Second {
		t.Errorf("gracePeriod = %v, want %v", exec.gracePeriod, 30*time.Second)
	}
	if exec.imagePullPolicy != "always" {
		t.Errorf("imagePullPolicy = %q, want %q", exec.imagePullPolicy, "always")
	}
	if exec.imageCacheTTL != 5*time.Minute {
		t.Errorf("imageCacheTTL = %v, want %v", exec.imageCacheTTL, 5*time.Minute)
	}

	// Cleanup
	_ = exec.docker.Close()
}

func TestGetRunningContainerID_NotFound(t *testing.T) {
	e := newTestExecutor(t)

	_, ok := e.GetRunningContainerID("nonexistent-project")
	if ok {
		t.Error("GetRunningContainerID should return false for unknown project")
	}
}

func TestGetRunningContainerID_Found(t *testing.T) {
	e := newTestExecutor(t)
	e.running["my-org/my-repo"] = "abc123def456"

	cid, ok := e.GetRunningContainerID("my-org/my-repo")
	if !ok {
		t.Error("GetRunningContainerID should return true for tracked project")
	}
	if cid != "abc123def456" {
		t.Errorf("container ID = %q, want %q", cid, "abc123def456")
	}
}

func TestStopContainer_NotFound(t *testing.T) {
	e := newTestExecutor(t)

	err := e.StopContainer(t.Context(), "nonexistent")
	if err == nil {
		t.Error("StopContainer should return error for unknown project")
	}
	if !strings.Contains(err.Error(), "no running container") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- helpers ---

func envSliceToMap(envs []string) map[string]string {
	m := make(map[string]string, len(envs))
	for _, e := range envs {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}

func assertEnv(t *testing.T, envMap map[string]string, key, expected string) {
	t.Helper()
	got, ok := envMap[key]
	if !ok {
		t.Errorf("expected env %s to be set, but it was not present", key)
		return
	}
	if got != expected {
		t.Errorf("env %s = %q, want %q", key, got, expected)
	}
}
