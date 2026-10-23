package quota

//test:in-package — pins what AccountQuotaOverrideSealCapabilityVersion 1 will
// assert: nothing in this package writes the override bucket except through
// PutAccountQuota's CAS loop, clearAccountQuota's guarded delete or
// SealAccountOverrides' CAS loop.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// overrideWriteFiles are the only files in this package that touch the
// override bucket. The package also holds the vCPU usage and Bedrock usage
// buckets, which write through their own stores and are out of scope here;
// scanning the whole package directory, as the EC2 settings equivalent does,
// would false-flag those unrelated writes.
var overrideWriteFiles = []string{"override.go", "seal.go"}

func quotaPackageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	return filepath.Dir(file)
}

// No file owning the override bucket may call Put, PutString, or Delete
// without a revision option: every write must go through PutAccountQuota's
// CAS loop, clearAccountQuota's guarded delete, or SealAccountOverrides' CAS
// loop.
func TestOverrideWritesAreRevisionGuarded(t *testing.T) {
	root := quotaPackageDir(t)
	fset := token.NewFileSet()
	scanned := 0

	for _, name := range overrideWriteFiles {
		path := filepath.Join(root, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
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
			switch sel.Sel.Name {
			case "Put", "PutString":
				t.Errorf("%s: unconditional %s call bypasses the revision-guarded override write; use PutAccountQuota or SealAccountOverrides instead",
					fset.Position(call.Pos()), sel.Sel.Name)
			case "Delete":
				if len(call.Args) < 3 {
					t.Errorf("%s: Delete call with no revision option bypasses the guarded clear; pin it to the read revision",
						fset.Position(call.Pos()))
				}
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatalf("scanned no override-write files under %s; the invariant would pass vacuously", root)
	}
}
