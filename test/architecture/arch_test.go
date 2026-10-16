// Package architecture enforces the dependency rules from ARCHITECTURE.md §1
// and CLAUDE.md using `go list`. It runs as part of `go test ./...`.
package architecture

import (
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/medbottle/betting-wallet-go"

// forbiddenInDomain are import path prefixes the domain must never reach,
// directly or transitively.
var forbiddenInDomain = []string{
	"go.uber.org/fx",
	"net/http",
	"github.com/aws/aws-sdk-go-v2",
	"github.com/jackc/pgx",
	"database/sql",
}

func TestDomainHasNoInfrastructureDependencies(t *testing.T) {
	deps := goList(t, "-deps", "-f", "{{.ImportPath}}", "./internal/domain/...")
	for _, v := range violations(deps, forbiddenInDomain) {
		t.Errorf("internal/domain depends on forbidden package %s", v)
	}
}

func TestAppDoesNotImportAdapters(t *testing.T) {
	deps := goList(t, "-deps", "-f", "{{.ImportPath}}", "./internal/app/...")
	for _, v := range violations(deps, []string{module + "/internal/adapters"}) {
		t.Errorf("internal/app depends on adapter package %s", v)
	}
}

// TestFxOnlyInBootstrapAndCmd checks direct imports: Fx is confined to
// internal/bootstrap and cmd/ (CLAUDE.md hard rule).
func TestFxOnlyInBootstrapAndCmd(t *testing.T) {
	lines := goList(t, "-f", `{{.ImportPath}} {{join .Imports " "}}`, "./...")
	for _, line := range lines {
		pkg, imports, _ := strings.Cut(line, " ")
		if pkg == module+"/internal/bootstrap" || strings.HasPrefix(pkg, module+"/cmd/") {
			continue
		}
		for _, v := range violations(strings.Fields(imports), []string{"go.uber.org/fx"}) {
			t.Errorf("%s imports %s; Fx belongs in internal/bootstrap or cmd/", pkg, v)
		}
	}
}

func TestViolationsMatchesPathBoundaries(t *testing.T) {
	deps := []string{
		"fmt",
		"net/http/httptest",
		"net/httpx",
		"github.com/jackc/pgx/v5",
		"github.com/aws/aws-sdk-go-v2/service/sqs",
		"database/sql/driver",
	}
	got := violations(deps, forbiddenInDomain)
	want := []string{"net/http/httptest", "github.com/jackc/pgx/v5", "github.com/aws/aws-sdk-go-v2/service/sqs", "database/sql/driver"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("violations = %v, want %v", got, want)
	}
}

// violations returns the entries of deps equal to, or nested under, any of
// the forbidden prefixes.
func violations(deps, forbidden []string) []string {
	var out []string
	for _, d := range deps {
		for _, f := range forbidden {
			if d == f || strings.HasPrefix(d, f+"/") {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("locate module root: %v", err)
	}
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = strings.TrimSpace(string(root))
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list %v: %v\n%s", args, err, ee.Stderr)
		}
		t.Fatalf("go list %v: %v", args, err)
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("go list %v returned nothing", args)
	}
	return lines
}
