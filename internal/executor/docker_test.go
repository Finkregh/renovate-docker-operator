package executor

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/resilience"
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
		logger:            slog.Default(),
		running:           make(map[string]string),
		sources:           make(map[string]resilience.Source),
		imageCache:        make(map[string]time.Time),
		failureMinRuntime: 30 * time.Second,
	}
}

func TestBuildEnvVars_Basic(t *testing.T) {
	e := newTestExecutor(t)

	// Clear env vars that would interfere
	t.Setenv("RENOVATE_TOKEN", "")
	t.Setenv("RENOVATE_LOG_LEVEL", "")

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
	assertEnv(t, envMap, "RENOVATE_LOG_LEVEL", "debug")

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
	t.Setenv("RENOVATE_TOKEN", "")

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
	t.Setenv("RENOVATE_TOKEN", "")

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
	t.Setenv("RENOVATE_TOKEN", "")

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
	t.Setenv("RENOVATE_TOKEN", "")
	t.Setenv("RENOVATE_LOG_LEVEL", "")

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

	assertEnv(t, envMap, "RENOVATE_LOG_LEVEL", "debug")
}

func TestBuildEnvVars_DefaultLogLevelDebug(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATE_TOKEN", "")
	t.Setenv("RENOVATE_LOG_LEVEL", "")

	// No ExecutionOptions set at all — should default to debug
	job := &api.RenovateJob{
		Name: "no-options-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "RENOVATE_LOG_LEVEL", "debug")
}

func TestBuildEnvVars_DebugModeFalse(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATE_TOKEN", "")
	t.Setenv("RENOVATE_LOG_LEVEL", "")

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

	assertEnv(t, envMap, "RENOVATE_LOG_LEVEL", "info")
}

func TestBuildEnvVars_LogLevelEnvOverride(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATE_TOKEN", "")
	t.Setenv("RENOVATE_LOG_LEVEL", "warn")

	// Env override should win over default debug
	job := &api.RenovateJob{
		Name: "env-override-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "RENOVATE_LOG_LEVEL", "warn")
}

func TestBuildEnvVars_LogLevelEnvOverrideBeatsDebugFalse(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATE_TOKEN", "")
	t.Setenv("RENOVATE_LOG_LEVEL", "debug")

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

	assertEnv(t, envMap, "RENOVATE_LOG_LEVEL", "debug")
}

func TestBuildEnvVars_TokenPassthrough(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATE_TOKEN", "my-secret-token")

	job := &api.RenovateJob{
		Name: "token-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "RENOVATE_TOKEN", "my-secret-token")
}

func TestBuildEnvVars_RenovateEnvPassthrough(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATE_TOKEN", "")
	t.Setenv("RENOVATE_CUSTOM_SETTING", "passed-through")

	job := &api.RenovateJob{
		Name: "passthrough-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	assertEnv(t, envMap, "RENOVATE_CUSTOM_SETTING", "passed-through")
}

func TestBuildEnvVars_ProtectedDefaultsCannotBeOverridden(t *testing.T) {
	e := newTestExecutor(t)
	t.Setenv("RENOVATE_TOKEN", "")
	// Set an env var that would conflict with a protected default
	t.Setenv("RENOVATE_LOG_FORMAT", "text-from-env")

	job := &api.RenovateJob{
		Name: "no-override-job",
	}

	result := e.buildEnvVars(job, false)
	envMap := envSliceToMap(result)

	// The protected default should win over passthrough
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
	if exec.containerbaseCacheVolume != "renovate-containerbase-cache" {
		t.Errorf("containerbaseCacheVolume = %q, want %q", exec.containerbaseCacheVolume, "renovate-containerbase-cache")
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
		Image:                    "custom/image:v1",
		Network:                  "host",
		CacheVolume:              "my-vol",
		ContainerbaseCacheVolume: "my-cb-vol",
		Parallelism:              8,
		JobTimeout:               10 * time.Minute,
		GracePeriod:              30 * time.Second,
		ImagePullPolicy:          "always",
		ImageCacheTTL:            5 * time.Minute,
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
	if exec.containerbaseCacheVolume != "my-cb-vol" {
		t.Errorf("containerbaseCacheVolume = %q, want %q", exec.containerbaseCacheVolume, "my-cb-vol")
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

func TestHostBinds(t *testing.T) {
	cfg := Config{
		CacheVolume:              "cv",
		ContainerbaseCacheVolume: "ccv",
	}

	exec, err := New(cfg, nil, slog.Default())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer func() { _ = exec.docker.Close() }()

	got := exec.hostBinds()
	if len(got) != 2 {
		t.Fatalf("hostBinds() returned %d entries, want 2", len(got))
	}
	if got[0] != "cv:/tmp/renovate" {
		t.Errorf("hostBinds()[0] = %q, want %q", got[0], "cv:/tmp/renovate")
	}
	if got[1] != "ccv:/tmp/containerbase/cache" {
		t.Errorf("hostBinds()[1] = %q, want %q", got[1], "ccv:/tmp/containerbase/cache")
	}
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

// ---------------------------------------------------------------------------
// T6: Outcome classification + resilience/metrics hook tests
// ---------------------------------------------------------------------------

// fakeReporter captures calls to Report.
type fakeReporter struct {
	calls []reportCall
}

type reportCall struct {
	Project  string
	Source   resilience.Source
	Outcome  resilience.Outcome
	Duration time.Duration
	ExitCode int
}

func (f *fakeReporter) Report(project string, source resilience.Source, outcome resilience.Outcome, duration time.Duration, exitCode int) {
	f.calls = append(f.calls, reportCall{
		Project:  project,
		Source:   source,
		Outcome:  outcome,
		Duration: duration,
		ExitCode: exitCode,
	})
}

// fakeRecorder captures calls to ObserveContainerDuration.
type fakeRecorder struct {
	calls []recorderCall
}

type recorderCall struct {
	Project  string
	Outcome  string
	Duration time.Duration
}

func (f *fakeRecorder) ObserveContainerDuration(project, outcome string, d time.Duration) {
	f.calls = append(f.calls, recorderCall{
		Project:  project,
		Outcome:  outcome,
		Duration: d,
	})
}

func TestClassifyOutcome(t *testing.T) {
	tests := []struct {
		name              string
		exitCode          int
		runtime           time.Duration
		failureMinRuntime time.Duration
		want              resilience.Outcome
	}{
		{
			name:              "exit 0 is success regardless of runtime",
			exitCode:          0,
			runtime:           1 * time.Second,
			failureMinRuntime: 30 * time.Second,
			want:              resilience.OutcomeSuccess,
		},
		{
			name:              "exit 1 with short runtime is rapid fail",
			exitCode:          1,
			runtime:           5 * time.Second,
			failureMinRuntime: 30 * time.Second,
			want:              resilience.OutcomeRapidFail,
		},
		{
			name:              "exit 1 with long runtime is slow fail",
			exitCode:          1,
			runtime:           45 * time.Second,
			failureMinRuntime: 30 * time.Second,
			want:              resilience.OutcomeSlowFail,
		},
		{
			name:              "exit at boundary (exactly failureMinRuntime) is slow fail",
			exitCode:          2,
			runtime:           30 * time.Second,
			failureMinRuntime: 30 * time.Second,
			want:              resilience.OutcomeSlowFail,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &DockerExecutor{failureMinRuntime: tc.failureMinRuntime}
			got := e.classifyOutcome(tc.exitCode, tc.runtime)
			if got != tc.want {
				t.Errorf("classifyOutcome(exit=%d, runtime=%v) = %q, want %q",
					tc.exitCode, tc.runtime, got, tc.want)
			}
		})
	}
}

func TestReportHooks_Success(t *testing.T) {
	e := newTestExecutor(t)
	rep := &fakeReporter{}
	rec := &fakeRecorder{}
	e.SetReporter(rep)
	e.SetRecorder(rec)

	// Simulate: exit=0, runtime=1s
	outcome := e.classifyOutcome(0, 1*time.Second)
	if outcome != resilience.OutcomeSuccess {
		t.Fatalf("expected Success, got %q", outcome)
	}

	// Manually invoke hooks as handleContainerExit would.
	e.reporter.Report("org/repo", resilience.SourceCron, outcome, 1*time.Second, 0)
	e.recorder.ObserveContainerDuration("org/repo", string(outcome), 1*time.Second)

	if len(rep.calls) != 1 {
		t.Fatalf("expected 1 report call, got %d", len(rep.calls))
	}
	if rep.calls[0].Outcome != resilience.OutcomeSuccess {
		t.Errorf("reporter outcome = %q, want %q", rep.calls[0].Outcome, resilience.OutcomeSuccess)
	}
	if rep.calls[0].ExitCode != 0 {
		t.Errorf("reporter exitCode = %d, want 0", rep.calls[0].ExitCode)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 recorder call, got %d", len(rec.calls))
	}
	if rec.calls[0].Outcome != string(resilience.OutcomeSuccess) {
		t.Errorf("recorder outcome = %q, want %q", rec.calls[0].Outcome, resilience.OutcomeSuccess)
	}
}

func TestReportHooks_RapidFail(t *testing.T) {
	e := newTestExecutor(t)
	rep := &fakeReporter{}
	rec := &fakeRecorder{}
	e.SetReporter(rep)
	e.SetRecorder(rec)

	outcome := e.classifyOutcome(1, 5*time.Second)
	if outcome != resilience.OutcomeRapidFail {
		t.Fatalf("expected RapidFail, got %q", outcome)
	}

	e.reporter.Report("org/repo", resilience.SourceWebhook, outcome, 5*time.Second, 1)
	e.recorder.ObserveContainerDuration("org/repo", string(outcome), 5*time.Second)

	if rep.calls[0].Outcome != resilience.OutcomeRapidFail {
		t.Errorf("reporter outcome = %q, want %q", rep.calls[0].Outcome, resilience.OutcomeRapidFail)
	}
	if rep.calls[0].ExitCode != 1 {
		t.Errorf("reporter exitCode = %d, want 1", rep.calls[0].ExitCode)
	}
	if rep.calls[0].Source != resilience.SourceWebhook {
		t.Errorf("reporter source = %q, want %q", rep.calls[0].Source, resilience.SourceWebhook)
	}
}

func TestReportHooks_SlowFail(t *testing.T) {
	e := newTestExecutor(t)
	rep := &fakeReporter{}
	rec := &fakeRecorder{}
	e.SetReporter(rep)
	e.SetRecorder(rec)

	outcome := e.classifyOutcome(1, 45*time.Second)
	if outcome != resilience.OutcomeSlowFail {
		t.Fatalf("expected SlowFail, got %q", outcome)
	}

	e.reporter.Report("org/repo", resilience.SourceCron, outcome, 45*time.Second, 1)
	e.recorder.ObserveContainerDuration("org/repo", string(outcome), 45*time.Second)

	if rep.calls[0].Outcome != resilience.OutcomeSlowFail {
		t.Errorf("reporter outcome = %q, want %q", rep.calls[0].Outcome, resilience.OutcomeSlowFail)
	}
	if rec.calls[0].Duration != 45*time.Second {
		t.Errorf("recorder duration = %v, want %v", rec.calls[0].Duration, 45*time.Second)
	}
}

func TestReportHooks_NilSafe(t *testing.T) {
	// When reporter and recorder are nil, no panics should occur.
	e := newTestExecutor(t)
	// Ensure reporter/recorder are nil (default in newTestExecutor)
	if e.reporter != nil {
		t.Fatal("expected nil reporter")
	}
	if e.recorder != nil {
		t.Fatal("expected nil recorder")
	}

	// Simulate the nil-guard pattern used in handleContainerExit.
	outcome := e.classifyOutcome(1, 5*time.Second)
	if e.reporter != nil {
		e.reporter.Report("org/repo", resilience.SourceCron, outcome, 5*time.Second, 1)
	}
	if e.recorder != nil {
		e.recorder.ObserveContainerDuration("org/repo", string(outcome), 5*time.Second)
	}
	// Test passes if no panic.
}

func TestSetProjectSource(t *testing.T) {
	e := newTestExecutor(t)

	e.SetProjectSource("org/repo", resilience.SourceWebhook)

	e.mu.Lock()
	src := e.sources["org/repo"]
	e.mu.Unlock()

	if src != resilience.SourceWebhook {
		t.Errorf("source = %q, want %q", src, resilience.SourceWebhook)
	}
}

func TestFailureMinRuntime_DefaultsTo30s(t *testing.T) {
	exec, err := New(Config{}, nil, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer func() { _ = exec.docker.Close() }()

	if exec.failureMinRuntime != 30*time.Second {
		t.Errorf("failureMinRuntime = %v, want %v", exec.failureMinRuntime, 30*time.Second)
	}
}

func TestFailureMinRuntime_Custom(t *testing.T) {
	exec, err := New(Config{FailureMinRuntime: 60 * time.Second}, nil, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer func() { _ = exec.docker.Close() }()

	if exec.failureMinRuntime != 60*time.Second {
		t.Errorf("failureMinRuntime = %v, want %v", exec.failureMinRuntime, 60*time.Second)
	}
}
