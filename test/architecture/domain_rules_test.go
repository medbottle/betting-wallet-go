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

// TestDomainHardRules parses every non-test file under internal/domain and
// fails on floating point (CLAUDE.md: never for money) and on panic
// (business rejections are errors).
func TestDomainHardRules(t *testing.T) {
	root := filepath.Join(moduleRoot(t), "internal", "domain")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, v := range hardRuleViolations(fset, f) {
			t.Errorf("%s", v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if len(got) != 4 { // float64 result, ParseFloat, float32, panic
		t.Fatalf("violations = %d, want 4:\n%s", len(got), strings.Join(got, "\n"))
	}
}

var floatFuncs = map[string]bool{"ParseFloat": true, "FormatFloat": true, "AppendFloat": true}

func hardRuleViolations(fset *token.FileSet, f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == "float32" || n.Name == "float64" {
				out = append(out, fset.Position(n.Pos()).String()+": floating point type "+n.Name)
			}
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "strconv" && floatFuncs[n.Sel.Name] {
				out = append(out, fset.Position(n.Pos()).String()+": strconv."+n.Sel.Name)
			}
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "panic" {
				out = append(out, fset.Position(n.Pos()).String()+": panic")
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
