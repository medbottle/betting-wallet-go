package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReadinessPendingUntilFirstRound(t *testing.T) {
	r := NewReadiness([]Check{{Name: "db", Fn: func(context.Context) error { return nil }}}, time.Second, time.Second, discard())
	rep := r.Report()
	if rep.Ready || rep.Checks["db"] != StatePending {
		t.Fatalf("want not ready/pending before first round, got %+v", rep)
	}
	r.CheckNow(context.Background())
	if rep := r.Report(); !rep.Ready || rep.Checks["db"] != StateOK {
		t.Fatalf("want ready after successful round, got %+v", rep)
	}
}

func TestReadinessFailingCheck(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	r := NewReadiness([]Check{
		{Name: "db", Fn: func(context.Context) error {
			if fail.Load() {
				return errors.New("down")
			}
			return nil
		}},
		{Name: "other", Fn: func(context.Context) error { return nil }},
	}, time.Second, time.Second, discard())

	r.CheckNow(context.Background())
	rep := r.Report()
	if rep.Ready || rep.Checks["db"] != StateFail || rep.Checks["other"] != StateOK {
		t.Fatalf("unexpected report: %+v", rep)
	}

	fail.Store(false)
	r.CheckNow(context.Background())
	if !r.Report().Ready {
		t.Fatal("want ready after recovery")
	}
}

func TestReadinessCheckTimeout(t *testing.T) {
	r := NewReadiness([]Check{{Name: "slow", Fn: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}}, time.Second, 20*time.Millisecond, discard())
	r.CheckNow(context.Background())
	if rep := r.Report(); rep.Ready || rep.Checks["slow"] != StateFail {
		t.Fatalf("want slow check to fail on timeout, got %+v", rep)
	}
}

func TestReadinessDraining(t *testing.T) {
	r := NewReadiness([]Check{{Name: "db", Fn: func(context.Context) error { return nil }}}, time.Second, time.Second, discard())
	r.CheckNow(context.Background())
	r.SetDraining()
	rep := r.Report()
	if rep.Ready || !rep.Draining {
		t.Fatalf("want draining and not ready, got %+v", rep)
	}
}

func TestReadinessRunStopsOnCancel(t *testing.T) {
	var calls atomic.Int32
	r := NewReadiness([]Check{{Name: "db", Fn: func(context.Context) error {
		calls.Add(1)
		return nil
	}}}, 5*time.Millisecond, 5*time.Millisecond, discard())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatal("loop did not tick")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
