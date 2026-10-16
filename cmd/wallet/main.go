// Command wallet runs the wager wallet service. Which roles run (HTTP,
// consumer, outbox relay, reference worker) is selected by RUN_* env vars.
package main

import (
	"go.uber.org/fx"

	"github.com/medbottle/betting-wallet-go/internal/bootstrap"
)

func main() {
	fx.New(bootstrap.Options()).Run()
}
