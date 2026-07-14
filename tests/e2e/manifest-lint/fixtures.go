package manifestlint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// createAPIs are resource-creating AWS SDK methods that should be reached
// through a harness.Ensure* fixture rather than called directly in a test, so
// setup is shared across the suite. Read/describe/delete/modify calls are not
// listed — only calls that bring a new resource into existence.
var createAPIs = map[string]bool{
	"RunInstances":                true,
	"CreateImage":                 true,
	"RegisterImage":               true,
	"CreateVpc":                   true,
	"CreateSubnet":                true,
	"CreateSecurityGroup":         true,
	"CreateKeyPair":               true,
	"ImportKeyPair":               true,
	"CreateLaunchTemplate":        true,
	"CreateLaunchTemplateVersion": true,
	"CreateVolume":                true,
	"CreateSnapshot":              true,
	"CreateNatGateway":            true,
	"CreateInternetGateway":       true,
	"CreateRouteTable":            true,
	"CreateLoadBalancer":          true,
	"CreateTargetGroup":           true,
	"CreateListener":              true,
	"AllocateAddress":             true,
	"CreateCluster":               true,
	"CreateNodegroup":             true,
	"CreateNetworkInterface":      true,
}

// awsClientFields are the AWSClient struct fields a create call must be made
// through to count (c.EC2.RunInstances, c.ELBv2.CreateLoadBalancer, ...).
// Anchoring on these keeps the lint from flagging unrelated Create* helpers.
var awsClientFields = map[string]bool{
	"EC2": true, "EC2Conf": true, "ELBv2": true, "IAM": true, "STS": true, "EKS": true,
}

// fixtureExcludeDirs are tests/e2e subdirectories that are not suites: the
// harness (where the Ensure* bodies legitimately live), shared libs, and the
// lint tooling itself.
var fixtureExcludeDirs = map[string]bool{
	"harness": true, "lib": true, "manifest-check": true,
	"manifest-lint": true, "select-suites": true, "_bin": true,
}

const allowCreateMarker = "e2e:allow-create"

// FixtureLint scans every suite test file under tests/e2e/*/ for direct
// resource-create AWS calls. repoRoot is the spinifex repo root.
func FixtureLint(repoRoot string) (*Result, error) {
	res := &Result{}
	base := filepath.Join(repoRoot, "tests", "e2e")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("read tests/e2e: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || fixtureExcludeDirs[e.Name()] {
			continue
		}
		if err := lintSuiteDir(filepath.Join(base, e.Name()), repoRoot, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func lintSuiteDir(dir, repoRoot string, res *Result) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if err := lintTestFile(filepath.Join(dir, e.Name()), repoRoot, res); err != nil {
			return err
		}
	}
	return nil
}

func lintTestFile(path, repoRoot string, res *Result) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	rel, _ := filepath.Rel(repoRoot, path)
	rel = filepath.ToSlash(rel)
	allowLines := allowMarkerLines(string(src))

	// Collect occurrences as (api, line) so ordinals are stable and independent
	// of which other APIs appear in the file.
	type occ struct {
		api  string
		line int
	}
	var occs []occ
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		api, ok := createCallAPI(call)
		if !ok {
			return true
		}
		line := fset.Position(call.Pos()).Line
		if allowLines[line] || allowLines[line-1] {
			return true
		}
		occs = append(occs, occ{api: api, line: line})
		return true
	})

	sort.Slice(occs, func(i, j int) bool {
		if occs[i].api != occs[j].api {
			return occs[i].api < occs[j].api
		}
		return occs[i].line < occs[j].line
	})
	ordinal := map[string]int{}
	for _, o := range occs {
		ordinal[o.api]++
		key := tab("create", rel, o.api, fmt.Sprintf("#%d", ordinal[o.api]))
		msg := fmt.Sprintf("%s:%d: direct %s — use a harness.Ensure* fixture or add // %s",
			rel, o.line, o.api, allowCreateMarker)
		res.add(key, msg)
	}
	return nil
}

// createCallAPI reports whether call is `<x>.<Field>.<CreateAPI>(...)` where
// Field is an AWSClient client field and CreateAPI is a resource-create method.
func createCallAPI(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr) // .<CreateAPI>
	if !ok || !createAPIs[sel.Sel.Name] {
		return "", false
	}
	field, ok := sel.X.(*ast.SelectorExpr) // .<Field>
	if !ok || !awsClientFields[field.Sel.Name] {
		return "", false
	}
	return sel.Sel.Name, true
}

// allowMarkerLines returns the set of 1-based line numbers containing the
// allow-create marker, so a call on (or one line below) the marker is exempt.
func allowMarkerLines(src string) map[int]bool {
	out := map[int]bool{}
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, allowCreateMarker) {
			out[i+1] = true
		}
	}
	return out
}
