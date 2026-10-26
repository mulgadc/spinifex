package account

//test:in-package — pins what AccountSettingsSealCapabilityVersion 1 asserts:
// nothing in this package writes the settings KV except through the
// revision-guarded helpers.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func accountPackageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	return filepath.Dir(file)
}

// The capability daemons report as version 1 is a claim about every writer in
// the binary, not just the responder: no non-test file in this package may
// call Put or PutString, which bypass the revision-guarded mutate/seal paths.
func TestAccountSettingsWritesAreRevisionGuarded(t *testing.T) {
	root := accountPackageDir(t)
	fset := token.NewFileSet()
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		scanned++

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Put" || sel.Sel.Name == "PutString" {
				t.Errorf("%s: unconditional %s call bypasses the revision-guarded settings write; use mutateSettings or SealAccount's CAS loop instead",
					fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatalf("scanned no Go files under %s; the invariant would pass vacuously", root)
	}
}
