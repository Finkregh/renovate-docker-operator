// Package scheduler provides a simple cron wrapper using robfig/cron.
package scheduler

import (
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler wraps a cron scheduler with structured logging.
type Scheduler struct {
	cron   *cron.Cron
	logger *slog.Logger
}

// New creates a new Scheduler instance.
func New(logger *slog.Logger) *Scheduler {
	return &Scheduler{
		cron:   cron.New(),
		logger: logger,
	}
}

// AddFunc adds a cron job with the given schedule expression and name.
func (s *Scheduler) AddFunc(schedule, name string, fn func()) error {
	s.logger.Info("adding cron schedule", "name", name, "schedule", schedule)
	_, err := s.cron.AddFunc(schedule, fn)
	return err
}

// Start begins the cron scheduler.
func (s *Scheduler) Start() {
	s.logger.Info("starting cron scheduler")
	s.cron.Start()
}

// Stop gracefully stops the cron scheduler and waits for running jobs to finish.
// If jobs don't complete within 60 seconds, it forces a return.
func (s *Scheduler) Stop() {
	s.logger.Info("stopping cron scheduler")
	ctx := s.cron.Stop()
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(done)
	}()
	select {
	case <-done:
		s.logger.Info("scheduler stopped gracefully")
	case <-time.After(60 * time.Second):
		s.logger.Warn("scheduler stop timed out after 60s, forcing shutdown")
	}
}

// GetNextRunOnSchedule parses a cron expression and returns the next time it fires.
func (s *Scheduler) GetNextRunOnSchedule(schedule string) time.Time {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(schedule)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(time.Now())
}
