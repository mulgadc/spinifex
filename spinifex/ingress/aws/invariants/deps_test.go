package invariants

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const modulePrefix = "github.com/mulgadc/spinifex/spinifex/"

// forbiddenRoots are the homes of domain and runtime implementations,
// including the legacy locations they have not yet left.
var forbiddenRoots = []string{
	"domains", "runtime", "gateway", "handlers", "services", "daemon", "vpcd",
}

func ingressAWSDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	return filepath.Dir(filepath.Dir(file))
}

// ADR-0002 S4: "ingress/aws imports neither domains/* nor runtime/*." ADR-0001
// rule 2 states the same for domain implementations.
func TestS4_IngressAWSImportsNoDomainOrRuntimeImplementation(t *testing.T) {
	root := ingressAWSDir(t)
	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		scanned++
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			rel, ok := strings.CutPrefix(p, modulePrefix)
			if !ok {
				continue
			}
			top, _, _ := strings.Cut(rel, "/")
			for _, bad := range forbiddenRoots {
				if top == bad {
					t.Errorf("ADR-0002 S4 \"ingress/aws imports neither domains/* nor runtime/*\": %s imports %s; take the capability through the registration contract wired by runtime/roles/awsgw instead",
						fset.Position(imp.Pos()), p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatalf("scanned no Go files under %s; the invariant would pass vacuously", root)
	}
}
