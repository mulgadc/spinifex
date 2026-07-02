// Command select-suites prints the e2e suites a diff must run.
//
// Usage:
//
//	select-suites --manifest docs/service-interfaces.yaml \
//	  --base-sha <sha> --head-sha <sha> [--repo-root .]
//
// Output (stdout, two lines, ready to eval into a workflow):
//
//	SUITES=e2e-cert e2e-lb e2e-single
//	RUN_PATTERN_e2e-single=^(TestVPCCRUD|TestSubnetCRUD)$
//
// A trailing comment with the selection reason goes to stderr. Exit non-zero
// only on hard errors (bad manifest, git failure); an empty diff or no match
// resolves to the configured default suites.
package main

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	manifestcheck "github.com/mulgadc/spinifex/tests/e2e/manifest-check"
	selectsuites "github.com/mulgadc/spinifex/tests/e2e/select-suites"
)

func main() {
	manifestPath := flag.String("manifest", "docs/service-interfaces.yaml", "path to service-interfaces.yaml")
	baseSHA := flag.String("base-sha", "", "diff base SHA (PR base)")
	headSHA := flag.String("head-sha", "", "diff head SHA")
	repoRoot := flag.String("repo-root", ".", "repo root for git diff + AST scan")
	noAST := flag.Bool("no-ast", false, "skip per-test AST narrowing")
	flag.Parse()

	if *baseSHA == "" || *headSHA == "" {
		fmt.Fprintln(os.Stderr, "select-suites: --base-sha and --head-sha are required")
		os.Exit(2)
	}

	m, err := manifestcheck.Load(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "select-suites: load manifest: %v\n", err)
		os.Exit(1)
	}

	changed, err := gitDiff(*repoRoot, *baseSHA, *headSHA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "select-suites: git diff: %v\n", err)
		os.Exit(1)
	}

	var perTest map[string]selectsuites.SuiteCoverage
	if !*noAST {
		perTest, err = selectsuites.ScanSuites(*repoRoot, m)
		if err != nil {
			// AST failure is non-fatal: fall back to whole-suite selection.
			fmt.Fprintf(os.Stderr, "select-suites: AST scan failed, running suites in full: %v\n", err)
			perTest = nil
		}
	}

	res := selectsuites.Select(m, selectsuites.DefaultConfig(), changed, perTest)

	fmt.Fprintf(os.Stderr, "# select-suites: %s\n", res.Reason)
	fmt.Printf("SUITES=%s\n", strings.Join(res.Suites, " "))
	for _, suite := range sortedMapKeys(res.RunPatterns) {
		fmt.Printf("RUN_PATTERN_%s=%s\n", suite, res.RunPatterns[suite])
	}
}

// gitDiff returns repo-root-relative changed paths between base and head.
func gitDiff(root, base, head string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "diff", "--name-only", base, head)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func sortedMapKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}
