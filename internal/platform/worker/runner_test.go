package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func logger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(&lockedWriter{w: buf}, nil))
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunnerProcessesThenIdles(t *testing.T) {
	var queue atomic.Int64
	queue.Store(25)
	var buf bytes.Buffer
	r := New(Config{Name: "test", Concurrency: 4, Idle: 10 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		if queue.Add(-1) >= 0 {
			return true, nil
		}
		return false, nil
	}, logger(&buf))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err == nil {
		t.Error("second Start must fail")
	}
	waitFor(t, func() bool { p, idle, _ := r.Stats(); return p == 25 && idle > 0 })
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Done():
	default:
		t.Fatal("Done not closed after Stop")
	}
	if !strings.Contains(buf.String(), `"msg":"worker stopped"`) || !strings.Contains(buf.String(), `"processed":25`) {
		t.Errorf("stop log missing counters: %s", buf.String())
	}
}

func TestRunnerCountsFailuresAndKeepsGoing(t *testing.T) {
	var calls atomic.Int64
	r := New(Config{Name: "flaky", Concurrency: 1, Idle: time.Millisecond}, func(ctx context.Context) (bool, error) {
		if calls.Add(1)%2 == 1 {
			return false, errors.New("boom")
		}
		return true, nil
	}, logger(&bytes.Buffer{}))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { p, _, f := r.Stats(); return p >= 3 && f >= 3 })
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerStopCancelsInFlightSteps(t *testing.T) {
	inStep := make(chan struct{}, 2)
	r := New(Config{Name: "slow", Concurrency: 2, Idle: time.Second}, func(ctx context.Context) (bool, error) {
		inStep <- struct{}{}
		<-ctx.Done() // a long step that honours cancellation
		return false, ctx.Err()
	}, logger(&bytes.Buffer{}))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	<-inStep
	<-inStep
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("stop took %v", time.Since(start))
	}
	if _, _, f := r.Stats(); f != 0 {
		t.Errorf("cancellation counted as %d failures", f)
	}
}

func TestRunnerStopTimesOut(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	r := New(Config{Name: "stuck", Concurrency: 1, Idle: time.Second}, func(ctx context.Context) (bool, error) {
		close(started)
		<-release // ignores ctx on purpose
		return false, nil
	}, logger(&bytes.Buffer{}))
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	close(release)
	<-r.Done()
}

func TestRunnerValidatesConfig(t *testing.T) {
	step := func(context.Context) (bool, error) { return false, nil }
	if err := New(Config{Name: "x", Concurrency: 0, Idle: time.Second}, step, logger(&bytes.Buffer{})).Start(); err == nil {
		t.Error("zero concurrency accepted")
	}
	if err := New(Config{Name: "x", Concurrency: 1}, step, logger(&bytes.Buffer{})).Start(); err == nil {
		t.Error("zero idle accepted")
	}
	if err := New(Config{Name: "x", Concurrency: 1, Idle: time.Second}, step, logger(&bytes.Buffer{})).Stop(context.Background()); err != nil {
		t.Errorf("stopping an unstarted runner: %v", err)
	}
}
