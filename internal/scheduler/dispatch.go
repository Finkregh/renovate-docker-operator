package scheduler

import (
	"log/slog"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/resilience"
)

// Gate decides whether a project dispatch should proceed.
// Implemented by [resilience.Manager].
type Gate interface {
	AllowDispatch(project string, source resilience.Source) (allowed bool, retryAfter time.Duration, reason string)
}

// DispatchMetrics records dispatch outcomes.
// Implemented by [metrics.Recorder].
type DispatchMetrics interface {
	RecordDispatch(project, result string)
	SetQueueDepth(n int)
}

// DispatchFunc is the callback invoked for each allowed project dispatch.
// The caller provides its own logic (state-store update, executor trigger, etc.).
type DispatchFunc func(project string) error

// Dispatcher applies the resilience gate to per-project dispatches
// and records metrics. It is safe to use with nil Gate or DispatchMetrics
// (both are treated as no-ops).
type Dispatcher struct {
	gate    Gate
	metrics DispatchMetrics
	logger  *slog.Logger
}

// NewDispatcher creates a Dispatcher. gate and metrics may be nil.
func NewDispatcher(gate Gate, metrics DispatchMetrics, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		gate:    gate,
		metrics: metrics,
		logger:  logger,
	}
}

// DispatchProjects iterates over the given projects and, for each one,
// checks the gate before invoking fn. It returns the count of projects
// that were actually dispatched and any error from fn (first error wins,
// but all projects are still attempted).
//
// source is passed to the gate; for cron-triggered dispatch use
// [resilience.SourceCron].
func (d *Dispatcher) DispatchProjects(projects []string, source resilience.Source, fn DispatchFunc) (dispatched int, firstErr error) {
	for _, project := range projects {
		if d.gate != nil {
			allowed, retryAfter, reason := d.gate.AllowDispatch(project, source)
			if !allowed {
				result := "skipped_" + reason
				d.logger.Info("dispatch skipped",
					"project", project,
					"reason", reason,
					"retryAfter", retryAfter,
				)
				if d.metrics != nil {
					d.metrics.RecordDispatch(project, result)
				}
				continue
			}
		}

		// Allowed — invoke the dispatch function.
		err := fn(project)
		if err != nil && firstErr == nil {
			firstErr = err
		}

		if d.metrics != nil {
			d.metrics.RecordDispatch(project, "dispatched")
		}
		dispatched++
	}
	return dispatched, firstErr
}
