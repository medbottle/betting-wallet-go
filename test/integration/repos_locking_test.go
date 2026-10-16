//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/medbottle/betting-wallet-go/internal/app"
)

// TestWalletLocking exercises the per-wallet pessimistic lock of
// ARCHITECTURE §4: the same wallet serialises, different wallets do not
// wait for each other, and lock_timeout turns a long wait into a transient
// error.
func TestWalletLocking(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)

	t.Run("same wallet waits for the holder", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		const hold = 500 * time.Millisecond
		locked := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- env.runner.InTx(context.Background(), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
				if _, err := r.Wallets().GetForUpdate(ctx, w.ID()); err != nil {
					return err
				}
				close(locked)
				time.Sleep(hold)
				return nil
			})
		}()
		<-locked
		start := time.Now()
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			_, err := r.Wallets().GetForUpdate(ctx, w.ID())
			return err
		})
		waited := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if waited < hold-100*time.Millisecond {
			t.Fatalf("second locker waited %v, want about %v", waited, hold)
		}
		if err := <-holderDone; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("different wallets do not wait", func(t *testing.T) {
		w1, w2 := env.openWallet(t, "100.00"), env.openWallet(t, "100.00")
		locked, release := make(chan struct{}), make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- env.runner.InTx(context.Background(), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
				if _, err := r.Wallets().GetForUpdate(ctx, w1.ID()); err != nil {
					return err
				}
				close(locked)
				<-release
				return nil
			})
		}()
		<-locked
		start := time.Now()
		err := env.runner.InTx(ctxT(t), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
			_, err := r.Wallets().GetForUpdate(ctx, w2.ID())
			return err
		})
		waited := time.Since(start)
		close(release)
		if err != nil {
			t.Fatal(err)
		}
		if waited > 300*time.Millisecond {
			t.Fatalf("wallet 2 waited %v while wallet 1 was locked", waited)
		}
		if err := <-holderDone; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("lock timeout is transient", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		release := holdTx(t, env.db.owner, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, w.ID())
		defer release()
		start := time.Now()
		err := env.runner.InTx(ctxT(t), app.TxOptions{LockTimeout: 200 * time.Millisecond}, func(ctx context.Context, r app.Repos) error {
			_, err := r.Wallets().GetForUpdate(ctx, w.ID())
			return err
		})
		if !errors.Is(err, app.ErrTransient) || pgCode(err) != "55P03" {
			t.Fatalf("err = %v, want ErrTransient (55P03)", err)
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Fatalf("lock_timeout not applied: waited %v", waited)
		}
	})

	t.Run("statement timeout is applied when set", func(t *testing.T) {
		w := env.openWallet(t, "100.00")
		release := holdTx(t, env.db.owner, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, w.ID())
		defer release()
		err := env.runner.InTx(ctxT(t), app.TxOptions{StatementTimeout: 200 * time.Millisecond}, func(ctx context.Context, r app.Repos) error {
			_, err := r.Wallets().GetForUpdate(ctx, w.ID())
			return err
		})
		// query_canceled; classified as transient (ARCHITECTURE §24, Q2).
		if pgCode(err) != "57014" {
			t.Fatalf("err = %v, want SQLSTATE 57014", err)
		}
	})
}

// TestNoDeadlockBetweenOperationsOnOneWallet races two different
// operations on the same wallet through the §4 flow (insert own row, then
// lock the wallet) many times. Each insert's foreign key check takes FOR
// KEY SHARE on the wallet; a FOR UPDATE wallet lock would conflict with it
// and deadlock (it did in 35 of 40 rounds). FOR NO KEY UPDATE must give
// zero deadlocks while still serialising the two writers.
func TestNoDeadlockBetweenOperationsOnOneWallet(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)
	const rounds = 40
	for round := 0; round < rounds; round++ {
		w := env.openWallet(t, "100.00")
		start := make(chan struct{})
		errs := make(chan error, 2)
		outcomes := make(chan string, 2)
		for i := 0; i < 2; i++ {
			go func(i int) {
				<-start
				errs <- env.runner.InTx(context.Background(), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
					tx, _, err := submit(ctx, r, env.ids, rawFor(w, "BET", "80.00", fmt.Sprintf("dl-%d-%d", round, i), ""), now())
					if err == nil {
						outcomes <- string(tx.Status())
					}
					return err
				})
			}(i)
		}
		close(start)
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: %v (SQLSTATE %s)", round, err, pgCode(err))
			}
		}
		got := map[string]int{<-outcomes: 1}
		got[<-outcomes]++
		if got["PROCESSED"] != 1 || got["REJECTED"] != 1 {
			t.Fatalf("round %d: outcomes %v, want one PROCESSED and one REJECTED", round, got)
		}
	}
}

// TestConcurrentDuplicateInsert: a duplicate InsertPending waits on the
// unique index until the first transaction finishes, then sees the outcome
// (ARCHITECTURE §6).
func TestConcurrentDuplicateInsert(t *testing.T) {
	t.Parallel()
	env := newRepoEnv(t)

	for _, firstCommits := range []bool{true, false} {
		name := "first rolls back: second inserts"
		if firstCommits {
			name = "first commits: second sees the duplicate"
		}
		t.Run(name, func(t *testing.T) {
			w := env.openWallet(t, "100.00")
			ext := "race-" + env.ids().String()
			raw := rawFor(w, "BET", "10.00", ext, "")
			inserted, release := make(chan struct{}), make(chan struct{})
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- env.runner.InTx(context.Background(), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
					if _, _, err := submit(ctx, r, env.ids, raw, now()); err != nil {
						return err
					}
					close(inserted)
					<-release
					if !firstCommits {
						return errors.New("abort first")
					}
					return nil
				})
			}()
			<-inserted

			secondDone := make(chan error, 1)
			go func() {
				secondDone <- env.runner.InTx(context.Background(), app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
					_, _, err := submit(ctx, r, env.ids, raw, now())
					return err
				})
			}()
			select {
			case err := <-secondDone:
				t.Fatalf("second insert did not wait for the first transaction: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			close(release)
			<-firstDone

			err := <-secondDone
			if firstCommits && !errors.Is(err, errDuplicate) {
				t.Fatalf("second = %v, want the duplicate detected", err)
			}
			if !firstCommits && err != nil {
				t.Fatalf("second = %v, want it to insert after the rollback", err)
			}
		})
	}
}
