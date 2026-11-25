package invariants

//test:in-package — these helpers are shared by every in-package test file here.

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Each test walks overlapping trees; parse each file once and share the
// read-only AST between the parallel tests.
type parsedFile struct {
	once sync.Once
	fset *token.FileSet
	file *ast.File
	err  error
}

var parseCache sync.Map // path -> *parsedFile

func parseGoFile(path string) (*token.FileSet, *ast.File, error) {
	v, ok := parseCache.Load(path)
	if !ok {
		v, _ = parseCache.LoadOrStore(path, &parsedFile{})
	}
	pf, ok := v.(*parsedFile)
	if !ok {
		return nil, nil, fmt.Errorf("parse cache holds %T for %s", v, path)
	}
	pf.once.Do(func() {
		pf.fset = token.NewFileSet()
		pf.file, pf.err = parser.ParseFile(pf.fset, path, nil, parser.SkipObjectResolution)
	})
	return pf.fset, pf.file, pf.err
}

// prewarmParse fills parseCache for paths on all CPUs; errors surface on the
// later parseGoFile call for the same path.
func prewarmParse(paths []string) {
	work := make(chan string)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for p := range work {
				_, _, _ = parseGoFile(p)
			}
		})
	}
	for _, p := range paths {
		work <- p
	}
	close(work)
	wg.Wait()
}

var (
	repoRootOnce sync.Once
	repoRootPath string
	errRepoRoot  error
)

// repoRoot returns the spinifex module root via `go env GOMOD`.
func repoRoot(t *testing.T) string {
	t.Helper()
	repoRootOnce.Do(func() {
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			errRepoRoot = fmt.Errorf("go env GOMOD: %w", err)
			return
		}
		gomod := strings.TrimSpace(string(out))
		if gomod == "" || gomod == "/dev/null" {
			errRepoRoot = fmt.Errorf("not in a go module (GOMOD=%q)", gomod)
			return
		}
		repoRootPath = strings.TrimSuffix(gomod, "/go.mod")
	})
	if errRepoRoot != nil {
		t.Fatalf("%v", errRepoRoot)
	}
	return repoRootPath
}

var (
	goListOnce sync.Once
	goListPkgs []goListPackage
	errGoList  error
)

// goListNetwork lists every package under networkRoot, invariants included.
func goListNetwork(t *testing.T) []goListPackage {
	t.Helper()
	root := repoRoot(t)
	goListOnce.Do(func() {
		cmd := exec.Command("go", "list", "-json=ImportPath,Imports", "./spinifex/domains/network/...")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				err = fmt.Errorf("%w: %s", err, ee.Stderr)
			}
			errGoList = fmt.Errorf("go list: %w", err)
			return
		}
		dec := json.NewDecoder(strings.NewReader(string(out)))
		for dec.More() {
			var p goListPackage
			if err := dec.Decode(&p); err != nil {
				errGoList = fmt.Errorf("decode go list output: %w", err)
				return
			}
			goListPkgs = append(goListPkgs, p)
		}
	})
	if errGoList != nil {
		t.Fatalf("%v", errGoList)
	}
	return goListPkgs
}
