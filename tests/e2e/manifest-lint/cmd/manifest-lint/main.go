// Command manifest-lint runs the e2e manifest drift guards (Bead 5):
// fixture-create lint + NATS subject lint, both ratcheted against a checked-in
// baseline.
//
// Usage:
//
//	manifest-lint [-repo-root .] [-manifest docs/service-interfaces.yaml]
//	              [-baseline tests/e2e/manifest-lint/baseline.txt] [-update]
//
// Exits non-zero on any NEW violation beyond the baseline. -update rewrites the
// baseline from the current findings (use after intentional changes).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	manifestcheck "github.com/mulgadc/spinifex/tests/e2e/manifest-check"
	manifestlint "github.com/mulgadc/spinifex/tests/e2e/manifest-lint"
)

func main() {
	var repoRoot, manifestPath, baselinePath string
	var update bool
	flag.StringVar(&repoRoot, "repo-root", ".", "spinifex repo root")
	flag.StringVar(&manifestPath, "manifest", "docs/service-interfaces.yaml", "path to the manifest YAML")
	flag.StringVar(&baselinePath, "baseline", "tests/e2e/manifest-lint/baseline.txt", "path to the ratchet baseline")
	flag.BoolVar(&update, "update", false, "rewrite the baseline from current findings")
	flag.Parse()

	root, err := filepath.Abs(repoRoot)
	if err != nil {
		failf("resolve repo-root: %v", err)
	}
	if !filepath.IsAbs(manifestPath) {
		manifestPath = filepath.Join(root, manifestPath)
	}
	if !filepath.IsAbs(baselinePath) {
		baselinePath = filepath.Join(root, baselinePath)
	}

	m, err := manifestcheck.Load(manifestPath)
	if err != nil {
		failf("%v", err)
	}

	fix, err := manifestlint.FixtureLint(root)
	if err != nil {
		failf("fixture lint: %v", err)
	}
	subj, err := manifestlint.SubjectLint(root, m)
	if err != nil {
		failf("subject lint: %v", err)
	}

	cands := append(append([]manifestlint.Violation{}, fix.Violations...), subj.Violations...)
	warnings := append(append([]string{}, fix.Warnings...), subj.Warnings...)

	if update {
		if err := manifestlint.WriteBaseline(baselinePath, manifestlint.AllKeys(cands)); err != nil {
			failf("write baseline: %v", err)
		}
		fmt.Printf("manifest-lint: baseline updated (%d entries) -> %s\n", len(cands), rel(root, baselinePath))
		return
	}

	baseline, err := manifestlint.LoadBaseline(baselinePath)
	if err != nil {
		failf("load baseline: %v", err)
	}
	newViol, stale := manifestlint.Filter(cands, baseline)

	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if len(stale) > 0 {
		fmt.Fprintf(os.Stderr, "note: %d baseline entr(ies) no longer hit — run `make manifest-lint-update` to slim:\n", len(stale))
		for _, s := range stale {
			fmt.Fprintf(os.Stderr, "  %s\n", s)
		}
	}

	if len(newViol) == 0 {
		fmt.Printf("manifest-lint: OK (%d baselined, %d warnings)\n", len(baseline), len(warnings))
		return
	}
	fmt.Fprintf(os.Stderr, "\nmanifest-lint: %d new violation(s):\n", len(newViol))
	for _, v := range newViol {
		fmt.Fprintf(os.Stderr, "  %s\n", v.Msg)
	}
	fmt.Fprintln(os.Stderr, "\nFix the call (use a harness.Ensure* fixture / declare the subject), add"+
		"\n// e2e:allow-create for an intentional create-path test, or run"+
		"\n`make manifest-lint-update` if this drift is accepted.")
	os.Exit(1)
}

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}
