package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDomainImportAllowList: internal/domain may depend only on the
// standard library, google/uuid and other domain packages (ARCHITECTURE §1).
func TestDomainImportAllowList(t *testing.T) {
	deps := goList(t, "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./internal/domain/...")
	for _, d := range deps {
		if d == "github.com/google/uuid" || d == module+"/internal/domain" || strings.HasPrefix(d, module+"/internal/domain/") {
			continue
		}
		t.Errorf("internal/domain depends on %s, outside the allow-list", d)
	}
}

// TestHardRules parses every non-test Go file of the module's code
// (internal/, cmd/, migrations/), so every package money flows through is
// covered, and fails on floating point (CLAUDE.md: never for money) and
// on panic (business rejections are errors). The only exceptions are in
// hardRuleAllowList, each with its reason; an entry nothing needs any more
// fails too, so the list stays exact.
func TestHardRules(t *testing.T) {
	root := moduleRoot(t)
	used := map[string]bool{}
	for _, dir := range []string{"internal", "cmd", "migrations"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(rel)
			for _, v := range hardRuleViolations(fset, f) {
				if _, ok := hardRuleAllowList[pkg][v.kind]; ok {
					used[pkg+" "+v.kind] = true
					continue
				}
				t.Errorf("%s", v)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for pkg, kinds := range hardRuleAllowList {
		if strings.HasPrefix(pkg, "internal/domain") {
			t.Errorf("%s: the domain has no exceptions to the hard rules", pkg)
		}
		for kind := range kinds {
			if !used[pkg+" "+kind] {
				t.Errorf("allow-list entry %s %s is no longer needed; remove it", pkg, kind)
			}
		}
	}
}

// hardRuleAllowList names, per package directory, the hard-rule constructs
// it may use and why. Money never reaches either.
var hardRuleAllowList = map[string]map[string]string{
	"internal/platform/metrics": {
		"float": "Prometheus records durations in seconds and counts as float64 (histogram buckets, gauge values)",
	},
	"internal/adapters/postgres": {
		"panic": "the TxRunner re-panics a recovered panic after rolling the transaction back; no rejection panics",
	},
}

func TestHardRuleCheckerDetects(t *testing.T) {
	src := `package x
import "strconv"
func f(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	var g float32
	_ = g
	if v < 0 { panic("negative") }
	return v
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := hardRuleViolations(fset, f)
	var kinds []string
	for _, v := range got {
		kinds = append(kinds, v.kind)
	}
	// float64 result, ParseFloat, float32, panic
	if strings.Join(kinds, ",") != "float,float,float,panic" {
		t.Fatalf("violations = %v, want float x3 and panic", got)
	}
}

var floatFuncs = map[string]bool{"ParseFloat": true, "FormatFloat": true, "AppendFloat": true}

// violation is one hard-rule construct: kind is "float" or "panic".
type violation struct {
	pos, kind, what string
}

func (v violation) String() string { return v.pos + ": " + v.what }

func hardRuleViolations(fset *token.FileSet, f *ast.File) []violation {
	var out []violation
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == "float32" || n.Name == "float64" {
				out = append(out, violation{fset.Position(n.Pos()).String(), "float", "floating point type " + n.Name})
			}
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "strconv" && floatFuncs[n.Sel.Name] {
				out = append(out, violation{fset.Position(n.Pos()).String(), "float", "strconv." + n.Sel.Name})
			}
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "panic" {
				out = append(out, violation{fset.Position(n.Pos()).String(), "panic", "panic"})
			}
		}
		return true
	})
	return out
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("locate module root: %v", err)
	}
	return strings.TrimSpace(string(out))
}
