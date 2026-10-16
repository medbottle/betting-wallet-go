// Package worker runs background loops with an observable lifecycle
// (ARCHITECTURE §14): its own context (never the start context), a fixed
// number of goroutines, an idle wait when there is nothing to do,
// cancellation on Stop, a "stopped" log with counters and a done channel.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Step does one unit of work. didWork false means nothing was due, and the
// goroutine waits for the idle interval before trying again. An error is
// logged and also followed by the idle wait. Step must honour ctx.
type Step func(ctx context.Context) (didWork bool, err error)

// Config describes a runner.
type Config struct {
	Name        string
	Concurrency int
	Idle        time.Duration
}

// Runner runs Step in Concurrency goroutines until stopped.
type Runner struct {
	cfg    Config
	step   Step
	logger *slog.Logger

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool

	processed atomic.Int64
	idle      atomic.Int64
	failures  atomic.Int64
}

// New returns an unstarted runner.
func New(cfg Config, step Step, logger *slog.Logger) *Runner {
	return &Runner{cfg: cfg, step: step, logger: logger.With(slog.String("worker", cfg.Name))}
}

// Start launches the goroutines. It returns at once.
func (r *Runner) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("worker %s: already started", r.cfg.Name)
	}
	if r.cfg.Concurrency < 1 || r.cfg.Idle <= 0 {
		return fmt.Errorf("worker %s: concurrency must be >= 1 and idle > 0", r.cfg.Name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done, r.started = cancel, make(chan struct{}), true

	var wg sync.WaitGroup
	for i := 0; i < r.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.loop(ctx)
		}()
	}
	go func() {
		wg.Wait()
		close(r.done)
	}()
	r.logger.Info("worker started", slog.Int("concurrency", r.cfg.Concurrency), slog.Duration("idle", r.cfg.Idle))
	return nil
}

func (r *Runner) loop(ctx context.Context) {
	for ctx.Err() == nil {
		did, err := r.step(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			r.failures.Add(1)
			r.logger.Warn("worker step failed", slog.String("error", err.Error()))
		case err != nil:
			return // cancelled mid-step: the step rolled back
		case did:
			r.processed.Add(1)
			continue
		default:
			r.idle.Add(1)
		}
		t := time.NewTimer(r.cfg.Idle)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Stop cancels the loops and waits for every goroutine to return, or for
// ctx to expire. In-flight steps see a cancelled context and roll back.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	r.cancel()
	done := r.done
	r.mu.Unlock()

	select {
	case <-done:
		r.logger.Info("worker stopped",
			slog.Int64("processed", r.processed.Load()), slog.Int64("idlePolls", r.idle.Load()), slog.Int64("failures", r.failures.Load()))
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker %s: stop: %w", r.cfg.Name, ctx.Err())
	}
}

// Done is closed once every goroutine has returned after Stop.
func (r *Runner) Done() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// Stats returns the counters: steps that did work, idle polls and failed
// steps.
func (r *Runner) Stats() (processed, idle, failures int64) {
	return r.processed.Load(), r.idle.Load(), r.failures.Load()
}
