// Package health tracks process readiness. A background loop runs the
// registered dependency checks periodically and caches the result, so
// /health/ready never blocks on a slow dependency. During shutdown the
// tracker is switched to draining, which makes readiness fail regardless
// of the checks (ARCHITECTURE §14).
package health

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Check is a named dependency probe. Fn must respect ctx cancellation.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// CheckState is the cached outcome of one check.
type CheckState string

const (
	// StatePending means the check has not completed yet.
	StatePending CheckState = "pending"
	// StateOK means the last run succeeded.
	StateOK CheckState = "ok"
	// StateFail means the last run failed.
	StateFail CheckState = "fail"
)

// Report is a snapshot of readiness.
type Report struct {
	Ready    bool
	Draining bool
	Checks   map[string]CheckState
}

// Readiness runs checks on an interval and serves the cached result.
type Readiness struct {
	checks   []Check
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger

	draining atomic.Bool

	mu     sync.RWMutex
	states map[string]CheckState
}

// NewReadiness builds a tracker. All checks start as pending, so the
// process is not ready until the first round completes.
func NewReadiness(checks []Check, interval, timeout time.Duration, logger *slog.Logger) *Readiness {
	states := make(map[string]CheckState, len(checks))
	for _, c := range checks {
		states[c.Name] = StatePending
	}
	return &Readiness{
		checks:   checks,
		interval: interval,
		timeout:  timeout,
		logger:   logger,
		states:   states,
	}
}

// Run executes the checks immediately and then on every interval until ctx
// is cancelled.
func (r *Readiness) Run(ctx context.Context) {
	r.CheckNow(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.CheckNow(ctx)
		}
	}
}

// CheckNow runs every check once, concurrently, each bounded by the
// per-check timeout, and stores the results.
func (r *Readiness) CheckNow(ctx context.Context) {
	results := make([]CheckState, len(r.checks))
	var wg sync.WaitGroup
	for i, c := range r.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			if err := c.Fn(cctx); err != nil {
				results[i] = StateFail
				if ctx.Err() == nil {
					r.logger.Warn("readiness check failed", slog.String("check", c.Name), slog.String("error", err.Error()))
				}
				return
			}
			results[i] = StateOK
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		// Cancelled mid-round: keep the previous snapshot rather than
		// recording failures caused by our own shutdown.
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.checks {
		prev := r.states[c.Name]
		r.states[c.Name] = results[i]
		if prev == StateFail && results[i] == StateOK {
			r.logger.Info("readiness check recovered", slog.String("check", c.Name))
		}
	}
}

// SetDraining marks the process as draining. It is irreversible for the
// lifetime of the process.
func (r *Readiness) SetDraining() {
	if !r.draining.Swap(true) {
		r.logger.Info("readiness draining")
	}
}

// Report returns the current snapshot.
func (r *Readiness) Report() Report {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rep := Report{
		Draining: r.draining.Load(),
		Checks:   make(map[string]CheckState, len(r.states)),
	}
	ready := !rep.Draining
	for name, s := range r.states {
		rep.Checks[name] = s
		if s != StateOK {
			ready = false
		}
	}
	rep.Ready = ready
	return rep
}
