// Command migrate applies or rolls back the embedded database migrations.
//
//	migrate up            apply all pending migrations
//	migrate down [N]      roll back N migrations (default 1)
//	migrate version       print the current version and dirty flag
//
// It connects with MIGRATE_DATABASE_URL, which must use the schema owner
// role (wallet_owner), not the application role.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/medbottle/wager-wallet-service/internal/adapters/postgres"
)

const usage = "usage: migrate up | down [N] | version"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv("MIGRATE_DATABASE_URL"), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, databaseURL string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	steps := 1
	switch args[0] {
	case "up", "version":
		if len(args) != 1 {
			return errors.New(usage)
		}
	case "down":
		if len(args) > 2 {
			return errors.New(usage)
		}
		if len(args) == 2 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				return fmt.Errorf("down: N must be a positive integer, got %q", args[1])
			}
			steps = n
		}
	default:
		return errors.New(usage)
	}
	if databaseURL == "" {
		return errors.New("MIGRATE_DATABASE_URL is required")
	}

	m, err := postgres.NewMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer m.Close()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	switch args[0] {
	case "up":
		err = m.Up(ctx)
	case "down":
		err = m.Down(ctx, steps)
	}
	if err != nil {
		return err
	}
	return printVersion(m, out)
}

func printVersion(m *postgres.Migrator, out io.Writer) error {
	v, dirty, err := m.Version()
	if errors.Is(err, postgres.ErrNoVersion) {
		fmt.Fprintln(out, "version: none")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "version: %d dirty: %t\n", v, dirty)
	return nil
}
