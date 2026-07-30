// Package metrics provides Prometheus metrics registration, recording helpers,
// and an HTTP middleware for the renovate-docker-operator.
//
// It uses a dedicated [prometheus.Registry] (never the global default) to
// keep tests isolated and avoid double-registration panics.
//
// The package supports three project-label cardinality modes controlled by
// [Config.ProjectLabelMode]:
//   - "all":     every project-scoped metric carries a `project` label.
//   - "breaker": only breaker-relevant metrics carry the `project` label.
//   - "off":     no metric carries the `project` label.
//
// Metric names are stable across all modes; only the label set differs.
package metrics

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ProjectLabelMode controls which metrics carry the `project` label.
type ProjectLabelMode string

const (
	// ModeAll attaches the `project` label to all project-scoped metrics.
	ModeAll ProjectLabelMode = "all"
	// ModeBreaker attaches `project` only to breaker-relevant metrics
	// (backoff, consecutive failures, container exits with failure outcomes).
	ModeBreaker ProjectLabelMode = "breaker"
	// ModeOff removes the `project` label from all metrics.
	ModeOff ProjectLabelMode = "off"
)

// ParseProjectLabelMode parses a mode string. Empty string defaults to ModeAll.
func ParseProjectLabelMode(s string) (ProjectLabelMode, error) {
	switch s {
	case "", "all":
		return ModeAll, nil
	case "breaker":
		return ModeBreaker, nil
	case "off":
		return ModeOff, nil
	default:
		return "", fmt.Errorf("metrics: unknown project label mode %q (valid: all, breaker, off)", s)
	}
}

// Config configures the metrics package.
type Config struct {
	// ProjectLabelMode controls per-project label cardinality. Default "all".
	ProjectLabelMode ProjectLabelMode
}

// Recorder holds the registered metrics and exposes recording helpers.
// Construct via [New]. Safe for concurrent use (prometheus collectors are
// goroutine-safe).
type Recorder struct {
	reg  *prometheus.Registry
	mode ProjectLabelMode
	http *httpMetrics

	// --- counters ---
	containerStarts  *prometheus.CounterVec
	containerExits   *prometheus.CounterVec
	rapidFailsTotal  prometheus.Counter
	dispatchTotal    *prometheus.CounterVec
	breakerTransitions *prometheus.CounterVec

	// --- histograms ---
	containerDuration *prometheus.HistogramVec

	// --- gauges ---
	projectsScheduled prometheus.Gauge
	projectsRunning   prometheus.Gauge
	discoveryRepos    *prometheus.GaugeVec
	discoveryDuration *prometheus.GaugeVec
	breakerOpen       prometheus.Gauge
	projectBackoff    *prometheus.GaugeVec
	projectFailures   *prometheus.GaugeVec
	queueDepth        prometheus.Gauge
}

// New creates a Recorder with all metrics registered on a fresh registry.
// Returns an error if [Config.ProjectLabelMode] is invalid.
func New(cfg Config) (*Recorder, error) {
	if cfg.ProjectLabelMode == "" {
		cfg.ProjectLabelMode = ModeAll
	}
	// Validate mode.
	switch cfg.ProjectLabelMode {
	case ModeAll, ModeBreaker, ModeOff:
	default:
		return nil, fmt.Errorf("metrics: unknown project label mode %q", cfg.ProjectLabelMode)
	}

	reg := prometheus.NewRegistry()
	// Include Go runtime and process collectors.
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	r := &Recorder{
		reg:  reg,
		mode: cfg.ProjectLabelMode,
	}

	// Helper: build label names depending on mode.
	// "project-scoped, always" → only for breaker-relevant metrics
	// "project-scoped, success path" → only in mode "all"
	projectLabelsAlways := func() []string {
		switch cfg.ProjectLabelMode {
		case ModeAll, ModeBreaker:
			return []string{"project"}
		default:
			return nil
		}
	}
	projectLabelsSuccessPath := func() []string {
		if cfg.ProjectLabelMode == ModeAll {
			return []string{"project"}
		}
		return nil
	}

	// --- renovate_container_starts_total ---
	startLabels := append([]string{"type", "job"}, projectLabelsSuccessPath()...)
	r.containerStarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "renovate_container_starts_total",
		Help: "Total number of started containers.",
	}, startLabels)
	reg.MustRegister(r.containerStarts)

	// --- renovate_container_exits_total ---
	// In "breaker" mode: failure outcomes carry project, success does not.
	// We solve this by always including the project label and using a
	// sentinel value when the mode drops it — but that violates Prometheus
	// semantics (label set must be consistent per metric family).
	// Correct approach: register WITH project always in "all" and "breaker"
	// (the caller will pass actual project in both cases for exits), and
	// WITHOUT project in "off".
	exitLabels := append([]string{"type", "job", "outcome"}, projectLabelsAlways()...)
	r.containerExits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "renovate_container_exits_total",
		Help: "Total number of exited containers by outcome.",
	}, exitLabels)
	reg.MustRegister(r.containerExits)

	// --- renovate_container_duration_seconds ---
	durationLabels := append([]string{"type", "job", "outcome"}, projectLabelsSuccessPath()...)
	r.containerDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "renovate_container_duration_seconds",
		Help:    "Container runtime distribution.",
		Buckets: prometheus.DefBuckets,
	}, durationLabels)
	reg.MustRegister(r.containerDuration)

	// --- renovate_projects_scheduled ---
	r.projectsScheduled = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "renovate_projects_scheduled",
		Help: "Current count of Scheduled projects.",
	})
	reg.MustRegister(r.projectsScheduled)

	// --- renovate_projects_running ---
	r.projectsRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "renovate_projects_running",
		Help: "Current count of Running projects.",
	})
	reg.MustRegister(r.projectsRunning)

	// --- renovate_discovery_repos ---
	r.discoveryRepos = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "renovate_discovery_repos",
		Help: "Last discovery repo count.",
	}, []string{"job"})
	reg.MustRegister(r.discoveryRepos)

	// --- renovate_discovery_last_duration_seconds ---
	r.discoveryDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "renovate_discovery_last_duration_seconds",
		Help: "Last discovery run duration in seconds.",
	}, []string{"job"})
	reg.MustRegister(r.discoveryDuration)

	// --- renovate_rapid_failures_total ---
	r.rapidFailsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "renovate_rapid_failures_total",
		Help: "Total rapid failures observed (feeds the breaker window).",
	})
	reg.MustRegister(r.rapidFailsTotal)

	// --- renovate_circuit_breaker_open ---
	r.breakerOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "renovate_circuit_breaker_open",
		Help: "1 if circuit breaker is open, 0 if closed.",
	})
	reg.MustRegister(r.breakerOpen)

	// --- renovate_circuit_breaker_transitions_total ---
	r.breakerTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "renovate_circuit_breaker_transitions_total",
		Help: "Circuit breaker state transitions.",
	}, []string{"to"})
	reg.MustRegister(r.breakerTransitions)

	// --- renovate_project_backoff_seconds ---
	// Always carries project label in all and breaker modes.
	backoffLabels := projectLabelsAlways()
	if len(backoffLabels) > 0 {
		r.projectBackoff = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "renovate_project_backoff_seconds",
			Help: "Remaining per-project backoff in seconds (0 if none).",
		}, backoffLabels)
	} else {
		// In "off" mode, register without project label — single aggregate gauge.
		r.projectBackoff = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "renovate_project_backoff_seconds",
			Help: "Remaining per-project backoff in seconds (0 if none).",
		}, []string{})
	}
	reg.MustRegister(r.projectBackoff)

	// --- renovate_project_consecutive_failures ---
	if len(projectLabelsAlways()) > 0 {
		r.projectFailures = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "renovate_project_consecutive_failures",
			Help: "Consecutive failure count per project.",
		}, projectLabelsAlways())
	} else {
		r.projectFailures = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "renovate_project_consecutive_failures",
			Help: "Consecutive failure count per project.",
		}, []string{})
	}
	reg.MustRegister(r.projectFailures)

	// --- renovate_dispatch_total ---
	dispatchLabels := append([]string{"result"}, projectLabelsSuccessPath()...)
	r.dispatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "renovate_dispatch_total",
		Help: "Total dispatch attempts by result.",
	}, dispatchLabels)
	reg.MustRegister(r.dispatchTotal)

	// --- renovate_queue_depth ---
	r.queueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "renovate_queue_depth",
		Help: "Current webhook replay queue depth.",
	})
	reg.MustRegister(r.queueDepth)

	return r, nil
}

// Handler returns an http.Handler that serves the Prometheus text exposition
// format (v0.0.4) for scraping.
func (r *Recorder) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		EnableOpenMetrics: false, // stick to text/plain v0.0.4
	})
}

// Registry returns the underlying prometheus registry. Useful for testutil.
func (r *Recorder) Registry() *prometheus.Registry {
	return r.reg
}

// Mode returns the active project-label mode.
func (r *Recorder) Mode() ProjectLabelMode {
	return r.mode
}

// ---------------------------------------------------------------------------
// Recording helpers. All accept `project` unconditionally; the mode determines
// whether the label is attached to the underlying metric.
// ---------------------------------------------------------------------------

// RecordContainerStart increments renovate_container_starts_total.
func (r *Recorder) RecordContainerStart(project, typ, job string) {
	labels := prometheus.Labels{"type": typ, "job": job}
	if r.hasProjectLabel(false) {
		labels["project"] = project
	}
	r.containerStarts.With(labels).Inc()
}

// RecordContainerExit increments renovate_container_exits_total.
// In "breaker" mode the project label is always attached for exits
// (they are breaker-relevant regardless of outcome).
func (r *Recorder) RecordContainerExit(project, typ, job, outcome string) {
	labels := prometheus.Labels{"type": typ, "job": job, "outcome": outcome}
	if r.hasProjectLabel(true) {
		labels["project"] = project
	}
	r.containerExits.With(labels).Inc()
}

// ObserveContainerDuration records a container duration observation.
func (r *Recorder) ObserveContainerDuration(project, outcome string, d time.Duration) {
	labels := prometheus.Labels{"type": "executor", "job": "renovate", "outcome": outcome}
	if r.hasProjectLabel(false) {
		labels["project"] = project
	}
	r.containerDuration.With(labels).Observe(d.Seconds())
}

// RecordDispatch increments renovate_dispatch_total.
func (r *Recorder) RecordDispatch(project, result string) {
	labels := prometheus.Labels{"result": result}
	if r.hasProjectLabel(false) {
		labels["project"] = project
	}
	r.dispatchTotal.With(labels).Inc()
}

// SetBreakerState sets the renovate_circuit_breaker_open gauge (1=open, 0=closed).
func (r *Recorder) SetBreakerState(state string) {
	switch state {
	case "open":
		r.breakerOpen.Set(1)
	default:
		r.breakerOpen.Set(0)
	}
}

// RecordBreakerTransition increments the transitions counter.
func (r *Recorder) RecordBreakerTransition(to string) {
	r.breakerTransitions.With(prometheus.Labels{"to": to}).Inc()
}

// SetProjectBackoff sets the renovate_project_backoff_seconds gauge.
func (r *Recorder) SetProjectBackoff(project string, seconds float64) {
	if r.hasProjectLabel(true) {
		r.projectBackoff.With(prometheus.Labels{"project": project}).Set(seconds)
	} else {
		// In "off" mode we can only set the aggregate; individual project info is lost.
		r.projectBackoff.With(prometheus.Labels{}).Set(seconds)
	}
}

// SetConsecutiveFailures sets the renovate_project_consecutive_failures gauge.
func (r *Recorder) SetConsecutiveFailures(project string, n int) {
	if r.hasProjectLabel(true) {
		r.projectFailures.With(prometheus.Labels{"project": project}).Set(float64(n))
	} else {
		r.projectFailures.With(prometheus.Labels{}).Set(float64(n))
	}
}

// RecordRapidFailure increments the renovate_rapid_failures_total counter.
func (r *Recorder) RecordRapidFailure() {
	r.rapidFailsTotal.Inc()
}

// RecordDiscovery records a discovery run result and duration.
// The `result` param is used as the `job` label for discovery gauges.
func (r *Recorder) RecordDiscovery(result string, d time.Duration) {
	r.discoveryDuration.With(prometheus.Labels{"job": result}).Set(d.Seconds())
}

// SetDiscoveryRepos sets the renovate_discovery_repos gauge.
func (r *Recorder) SetDiscoveryRepos(job string, count int) {
	r.discoveryRepos.With(prometheus.Labels{"job": job}).Set(float64(count))
}

// SetProjectsScheduled sets the renovate_projects_scheduled gauge.
func (r *Recorder) SetProjectsScheduled(n int) {
	r.projectsScheduled.Set(float64(n))
}

// SetProjectsRunning sets the renovate_projects_running gauge.
func (r *Recorder) SetProjectsRunning(n int) {
	r.projectsRunning.Set(float64(n))
}

// SetQueueDepth sets the renovate_queue_depth gauge (webhook replay queue).
func (r *Recorder) SetQueueDepth(n int) {
	r.queueDepth.Set(float64(n))
}

// hasProjectLabel reports whether a project label should be attached for the
// current mode. `breakerRelevant` distinguishes between metrics that always
// carry the label in breaker mode and those that only carry it in "all" mode.
func (r *Recorder) hasProjectLabel(breakerRelevant bool) bool {
	switch r.mode {
	case ModeAll:
		return true
	case ModeBreaker:
		return breakerRelevant
	default:
		return false
	}
}

// ErrInvalidMode is returned when an unknown mode string is provided.
var ErrInvalidMode = errors.New("metrics: invalid project label mode")
