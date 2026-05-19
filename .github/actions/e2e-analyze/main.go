package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

func main() {
	var junitGlob, logDir, title string
	flag.StringVar(&junitGlob, "junit-glob", os.Getenv("ANALYZE_JUNIT_GLOB"), "glob matching junit-*.xml files")
	flag.StringVar(&logDir, "log-dir", os.Getenv("ANALYZE_LOG_DIR"), "artifact dir to write analysis.md into")
	flag.StringVar(&title, "title", os.Getenv("ANALYZE_TITLE"), "report heading")
	flag.Parse()

	if title == "" {
		title = "E2E failure analysis"
	}
	if junitGlob == "" {
		fmt.Fprintln(os.Stderr, "::error::e2e-analyze: -junit-glob is required")
		os.Exit(0) // analyzer is advisory only; existing gate owns exit code
	}

	paths, err := filepath.Glob(junitGlob)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: bad glob %q: %v\n", junitGlob, err)
		os.Exit(0)
	}
	sort.Strings(paths)

	report := Report{Title: title}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: read %s: %v\n", p, err)
			continue
		}
		sr, err := ParseFile(p, data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: %v\n", err)
			continue
		}
		report.Suites = append(report.Suites, sr)
	}

	// Stage 2: materialise per-failure bundles next to the JUnit data
	// before rendering so Render can link each failure to its bundle.
	if logDir != "" {
		WriteBundles(&report, logDir)
	}

	out := Render(report)

	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o755); err == nil {
			if err := os.WriteFile(filepath.Join(logDir, "analysis.md"), []byte(out), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: write analysis.md: %v\n", err)
			}
		}
	}

	if summaryPath := os.Getenv("GITHUB_STEP_SUMMARY"); summaryPath != "" {
		f, err := os.OpenFile(summaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			defer f.Close()
			if _, werr := f.WriteString(out); werr != nil {
				fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: append summary: %v\n", werr)
			}
		}
	} else {
		// Local invocation (developer running the tool by hand). Mirror to stdout.
		fmt.Print(out)
	}
}
