package accountteardown

//test:in-package — pins ADR-0003:S4 and ADR-0001 Context item 2 for the EC2
// settings seal: the legacy engine must reach the owner only through the
// versioned contract and NATS, never by reading the settings bucket itself.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// forbiddenSettingsImports are imports that would let this package reach the
// EC2 settings owner or the configured node set by a path other than the
// versioned contract and NewClusterEngine's parameters.
var forbiddenSettingsImports = []string{
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/account",
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config",
}

// forbiddenSettingsIdentifiers would let this package name the settings
// bucket or its key directly instead of reaching it only through
// contracts/ec2/v1 and NATS.
var forbiddenSettingsIdentifiers = []string{"spinifex-ec2-account-settings", "KVBucketAccountSettings"}

// Teardown reaches EC2 settings only over contracts/ec2/v1: no import of the
// settings owner or bootstrap/config (nodes arrive via NewClusterEngine), and
// no literal naming the settings bucket or its constant.
func TestAccountTeardownHasNoSettingsKVAccess(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	root := filepath.Dir(file)

	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		scanned++

		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbiddenSettingsImports {
				if p == bad {
					t.Errorf("%s: imports %s; the legacy engine must reach the EC2 settings owner only through contracts/ec2/v1 and the configured node set NewClusterEngine already receives",
						fset.Position(imp.Pos()), p)
				}
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			for _, bad := range forbiddenSettingsIdentifiers {
				if value == bad {
					t.Errorf("%s: names %q; the settings bucket and its key are the owner's own, never this package's",
						fset.Position(lit.Pos()), value)
				}
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
