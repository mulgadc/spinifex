//go:build e2e

package scenarios

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// scenarioFuncRe matches TestScenario<L>_<Name> functions; capture group is the scenario letter.
var scenarioFuncRe = regexp.MustCompile(`^TestScenario([A-Z])_[A-Za-z0-9]+$`)

// coverageDocRe matches the scenario-letter column in the DDIL table (e.g. `| A |`),
// anchored on a leading pipe to avoid matching prose mentions.
var coverageDocRe = regexp.MustCompile(`(?m)^\|\s*([A-F])\s*\|`)

// TestCoverageDrift enforces TEST_COVERAGE.md and the scenarios package stay in sync:
// fails if a TestScenario<L>_... function exists without a matching DDIL table row or
// vice versa. Runs unconditionally (no cluster dependency).
func TestCoverageDrift(t *testing.T) {
	scenarioDir := callerDir(t)

	codeLetters, err := lettersFromSource(scenarioDir)
	if err != nil {
		t.Fatalf("extract scenario letters from source: %v", err)
	}
	if len(codeLetters) == 0 {
		t.Fatalf("no TestScenario<L>_... functions found in %s — coverage drift check is meaningful only with scenarios present", scenarioDir)
	}

	docPath := filepath.Join(scenarioDir, "..", "..", "TEST_COVERAGE.md")
	docLetters, err := lettersFromDoc(docPath)
	if err != nil {
		t.Fatalf("extract scenario letters from %s: %v", docPath, err)
	}

	missing := setDiff(codeLetters, docLetters)
	extra := setDiff(docLetters, codeLetters)
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	if len(missing) > 0 {
		t.Errorf("scenarios present in code but missing from %s DDIL table: %v", filepath.Base(docPath), missing)
	}
	if len(extra) > 0 {
		t.Errorf("scenarios present in %s DDIL table but missing TestScenario<L>_... in code: %v", filepath.Base(docPath), extra)
	}
}

// callerDir returns the directory of this test file, used to locate siblings and TEST_COVERAGE.md.
func callerDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed — cannot locate scenarios dir")
	}
	return filepath.Dir(self)
}

// lettersFromSource AST-parses scenario *_test.go files and returns the deduplicated
// sorted set of letters declared by top-level TestScenario<L>_... functions.
func lettersFromSource(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	seen := make(map[string]struct{})
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "coverage_drift_test.go" || name == "main_test.go" {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			m := scenarioFuncRe.FindStringSubmatch(fn.Name.Name)
			if m == nil {
				continue
			}
			seen[m[1]] = struct{}{}
		}
	}
	return sortedKeys(seen), nil
}

// lettersFromDoc reads TEST_COVERAGE.md and returns deduplicated sorted scenario letters
// from its DDIL table (matched by coverageDocRe, ignoring prose mentions).
func lettersFromDoc(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	for _, m := range coverageDocRe.FindAllStringSubmatch(string(data), -1) {
		seen[m[1]] = struct{}{}
	}
	return sortedKeys(seen), nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// setDiff returns elements in a that are not in b (both inputs must be sorted).
func setDiff(a, b []string) []string {
	var out []string
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			j++
		default:
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	return out
}
