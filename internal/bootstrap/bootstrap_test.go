package bootstrap

import (
	"testing"

	"go.uber.org/fx"
)

// TestGraphIsComplete validates the dependency graph without running any
// constructor, so it needs no database. The real start/stop test lives in
// test/integration.
func TestGraphIsComplete(t *testing.T) {
	if err := fx.ValidateApp(Options()); err != nil {
		t.Fatalf("fx graph is invalid: %v", err)
	}
}
