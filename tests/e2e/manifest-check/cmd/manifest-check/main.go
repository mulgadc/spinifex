// Command manifest-check validates spinifex/docs/service-interfaces.yaml.
//
// Usage: manifest-check [-manifest path] [-repo-root path]
//
// Exits non-zero with one error per line on validation failure.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	manifestcheck "github.com/mulgadc/spinifex/tests/e2e/manifest-check"
)

func main() {
	var manifestPath, repoRoot string
	flag.StringVar(&manifestPath, "manifest", "docs/service-interfaces.yaml", "path to the manifest YAML")
	flag.StringVar(&repoRoot, "repo-root", ".", "spinifex repo root for resolving service/suite paths")
	flag.Parse()

	root, err := filepath.Abs(repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve repo-root: %v\n", err)
		os.Exit(2)
	}
	if !filepath.IsAbs(manifestPath) {
		manifestPath = filepath.Join(root, manifestPath)
	}

	m, err := manifestcheck.Load(manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	errs := manifestcheck.Validate(m, root)
	if len(errs) == 0 {
		fmt.Printf("manifest-check: %s OK (%d services, %d suites)\n", manifestPath, len(m.Services), len(m.Suites))
		return
	}
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, e)
	}
	os.Exit(1)
}
