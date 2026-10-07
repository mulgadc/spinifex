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

// compositionRoot is the one package ADR-0002 S4 lets wire a domain's
// registration into the ingress contract.
const compositionRoot = "spinifex/runtime/roles/awsgw"

// declaredInventoryReaders import both packages only to read a registration's
// declared operation inventory; they serve no request, so compose nothing.
var declaredInventoryReaders = map[string]bool{
	"cmd/aws-model-coverage": true,
}

// ADR-0002 acceptance 3: the awsgw role is the only production composition
// point that imports both the registration contract and ECR implementation.
func TestS4_OnlyAWSGWRoleImportsDispatchAndECRAWSAPI(t *testing.T) {
	const (
		contract = modulePrefix + "ingress/aws/dispatch"
		ecrImpl  = modulePrefix + "domains/ecr/awsapi"
	)
	moduleRoot := filepath.Dir(filepath.Dir(filepath.Dir(ingressAWSDir(t))))
	imports := map[string]map[string]token.Position{}
	fset := token.NewFileSet()
	for _, top := range []string{"cmd", "contracts", "internal", "spinifex"} {
		err := filepath.WalkDir(filepath.Join(moduleRoot, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "vendor" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(moduleRoot, filepath.Dir(path))
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(rel)
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if p != contract && p != ecrImpl {
					continue
				}
				if imports[pkg] == nil {
					imports[pkg] = map[string]token.Position{}
				}
				imports[pkg][p] = fset.Position(imp.Pos())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(imports[compositionRoot]) != 2 {
		t.Fatalf("%s no longer imports both %s and %s; the invariant would pass vacuously", compositionRoot, contract, ecrImpl)
	}
	for pkg, found := range imports {
		if len(found) < 2 || pkg == compositionRoot || declaredInventoryReaders[pkg] {
			continue
		}
		t.Errorf("ADR-0002 acceptance 3 \"The awsgw role is the only production composition point that imports both the registration contract and ECR implementation\": %s imports %s (%s) and %s (%s); register ECR in runtime/roles/awsgw instead",
			pkg, contract, found[contract], ecrImpl, found[ecrImpl])
	}
}
