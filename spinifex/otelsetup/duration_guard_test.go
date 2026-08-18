package otelsetup_test

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/mulgadc/spinifex/spinifex/otelsetup"
)

// logMethods are the slog entry points that take loosely typed key/value pairs,
// where a duration argument carries no unit of its own.
var logMethods = map[string]bool{
	"Debug": true, "DebugContext": true,
	"Info": true, "InfoContext": true,
	"Warn": true, "WarnContext": true,
	"Error": true, "ErrorContext": true,
	"Log": true, "LogAttrs": true,
}

// TestSlogDurationConvention enforces the one duration convention: every
// duration logged goes through otelsetup.Millis under a key ending in "_ms".
// A bare time.Duration is logged as unitless nanoseconds, so without this two
// duration fields in the same index can differ by a factor of a billion with
// nothing in the record to say so.
func TestSlogDurationConvention(t *testing.T) {
	root := moduleRoot(t)

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
		Dir: root,
	}
	pkgs, err := packages.Load(cfg, "./spinifex/...", "./cmd/...")
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}

	var raw, unkeyed []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isLogCall(pkg.TypesInfo, call) {
					return true
				}
				for i, arg := range call.Args {
					switch {
					case isDuration(pkg.TypesInfo.TypeOf(arg)):
						raw = append(raw, site(root, pkg.Fset, arg.Pos()))
					case isMillisCall(pkg.TypesInfo, arg) && !precededByUnitKey(call.Args, i):
						unkeyed = append(unkeyed, site(root, pkg.Fset, arg.Pos()))
					}
				}
				return true
			})
		}
	}

	if len(raw) > 0 {
		t.Errorf("slog call sites pass a raw time.Duration:\n  %s\n"+
			"Log it as \"<name>%s\", otelsetup.Millis(d) so the field carries its unit.",
			strings.Join(raw, "\n  "), otelsetup.DurationSuffix)
	}
	if len(unkeyed) > 0 {
		t.Errorf("otelsetup.Millis logged under a key not ending in %q:\n  %s",
			otelsetup.DurationSuffix, strings.Join(unkeyed, "\n  "))
	}
}

// isLogCall reports whether call is a slog logging call or slog.Duration, both
// of which would take a duration argument without recording its unit.
func isLogCall(info *types.Info, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := info.ObjectOf(sel.Sel).(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "log/slog" {
		return false
	}
	return logMethods[sel.Sel.Name] || sel.Sel.Name == "Duration"
}

// isMillisCall reports whether expr is a call to otelsetup.Millis.
func isMillisCall(info *types.Info, expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := info.ObjectOf(sel.Sel).(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	return fn.Name() == "Millis" &&
		fn.Pkg().Path() == "github.com/mulgadc/spinifex/spinifex/otelsetup"
}

// precededByUnitKey reports whether the argument at i is the value half of a
// key/value pair whose key literal ends in the duration suffix.
func precededByUnitKey(args []ast.Expr, i int) bool {
	if i == 0 {
		return false
	}
	lit, ok := args[i-1].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	key, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	return strings.HasSuffix(key, otelsetup.DurationSuffix)
}

// isDuration reports whether t is exactly time.Duration.
func isDuration(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "time" && obj.Name() == "Duration"
}

// site renders pos as a module-relative file:line.
func site(root string, fset *token.FileSet, pos token.Pos) string {
	p := fset.Position(pos)
	rel, err := filepath.Rel(root, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	return rel + ":" + strconv.Itoa(p.Line)
}

// moduleRoot walks up from the test's directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}
