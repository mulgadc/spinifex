package selectsuites

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SuiteCoverage records, per suite, which services each top-level Test*
// function exercises. Coverage is derived by walking the suite's package call
// graph from each Test to the harness.Ensure* helpers it reaches, then mapping
// those fixtures to services via the manifest's fixtures block.
type SuiteCoverage struct {
	Path  string
	Tests map[string][]string // test func name → sorted service names
}

// ScanSuites builds per-test coverage for every suite in the manifest. repoRoot
// is the spinifex repo root (suite paths are resolved beneath it). Suites whose
// directory is missing or has no test files are skipped silently — the selector
// then runs them in full.
func ScanSuites(repoRoot string, m *Manifest) (map[string]SuiteCoverage, error) {
	fixSvc := map[string][]string{}
	for name, f := range m.Fixtures {
		fixSvc[name] = f.Services
	}
	out := map[string]SuiteCoverage{}
	for name, s := range m.Suites {
		dir := filepath.Join(repoRoot, filepath.FromSlash(s.Path))
		cov, err := scanSuiteDir(dir, fixSvc)
		if err != nil {
			return nil, err
		}
		if cov == nil {
			continue
		}
		out[name] = SuiteCoverage{Path: s.Path, Tests: cov}
	}
	return out, nil
}

// scanSuiteDir parses every *_test.go in dir, builds the local call graph, and
// returns testName → services. Returns nil (not an error) when the dir has no
// Go test files.
func scanSuiteDir(dir string, fixSvc map[string][]string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	fset := token.NewFileSet()
	// directEnsure[func] = set of Ensure* fixture names called directly in func.
	directEnsure := map[string]map[string]bool{}
	// localCalls[func] = set of package-local func names called in func.
	localCalls := map[string]map[string]bool{}
	tests := map[string]bool{}
	found := false

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		found = true
		f, perr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if perr != nil {
			return nil, perr
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			name := fn.Name.Name
			if isTestFunc(fn) {
				tests[name] = true
			}
			de, lc := scanFuncBody(fn.Body)
			directEnsure[name] = de
			localCalls[name] = lc
		}
	}
	if !found {
		return nil, nil
	}

	out := map[string][]string{}
	for t := range tests {
		ensure := reachableEnsure(t, directEnsure, localCalls)
		svc := map[string]bool{}
		for fx := range ensure {
			for _, s := range fixSvc[fx] {
				svc[s] = true
			}
		}
		out[t] = sortedKeys(svc)
	}
	return out, nil
}

// scanFuncBody collects the harness.Ensure* fixture calls and package-local
// function calls made directly inside body.
func scanFuncBody(body *ast.BlockStmt) (ensure, local map[string]bool) {
	ensure = map[string]bool{}
	local = map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "harness" &&
				strings.HasPrefix(fn.Sel.Name, "Ensure") {
				ensure[fn.Sel.Name] = true
			}
		case *ast.Ident:
			local[fn.Name] = true
		}
		return true
	})
	return ensure, local
}

// reachableEnsure returns every Ensure* fixture reachable from start by
// following package-local calls (BFS over the local call graph).
func reachableEnsure(start string, direct, local map[string]map[string]bool) map[string]bool {
	seen := map[string]bool{}
	ensure := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if seen[fn] {
			continue
		}
		seen[fn] = true
		for e := range direct[fn] {
			ensure[e] = true
		}
		for callee := range local[fn] {
			if !seen[callee] {
				queue = append(queue, callee)
			}
		}
	}
	return ensure
}

func isTestFunc(fn *ast.FuncDecl) bool {
	if !strings.HasPrefix(fn.Name.Name, "Test") {
		return false
	}
	// Exclude TestMain — it is the process entry, not a coverage unit.
	return fn.Name.Name != "TestMain"
}

// runPatterns builds a suite→regex map keeping only tests that touch a covered
// service (tests with no analyzable fixture calls are always kept, so narrowing
// never drops an unanalyzable test). A pattern is emitted only when it is a
// strict, non-empty subset of the suite's tests.
func runPatterns(perTest map[string]SuiteCoverage, suites []string, covered map[string]bool) map[string]string {
	out := map[string]string{}
	for _, suite := range suites {
		cov, ok := perTest[suite]
		if !ok || len(cov.Tests) == 0 {
			continue
		}
		var keep []string
		for t, svcs := range cov.Tests {
			if len(svcs) == 0 || intersects(svcs, covered) {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 || len(keep) >= len(cov.Tests) {
			continue // empty or no narrowing — run the whole suite
		}
		sort.Strings(keep)
		out[suite] = "^(" + strings.Join(keep, "|") + ")$"
	}
	return out
}

func intersects(svcs []string, set map[string]bool) bool {
	for _, s := range svcs {
		if set[s] {
			return true
		}
	}
	return false
}
