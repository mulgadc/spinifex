package main

// Stage 2 of docs/development/improvements/e2e-go-failure-analysis.md:
// for each failure surfaced by Stage 1, materialise a per-failure bundle
// directory containing the failure summary, a time-window slice of the
// daemon journal, a testcase-window slice of the suite's stdout log, and
// any per-test artifacts the harness already dumped at run-time.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// bundlePad is the per-failure window padding either side of the
// [StartAt, StartAt+Duration] testcase span. Five seconds matches the
// plan doc: long enough to catch the daemon's pre-test setup and
// post-test cleanup chatter, short enough to keep the slice focused.
const bundlePad = 5 * time.Second

// nameSanitiseRe replaces characters unsafe in directory names with `_`.
// Tracks tests/e2e/harness/artifacts.go which uses the same convention
// when the test's own dump dir is created, so we can pick its contents
// up by path without a second translation table.
var nameSanitiseRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitiseTestName(s string) string {
	return nameSanitiseRe.ReplaceAllString(s, "_")
}

// ApplySuiteStartFiles overrides each failure's StartAt by reading
// <logDir>/test-<suiteLabel>.start (RFC3339 timestamp written by the
// workflow before the suite runs). Required because go-junit-report's
// `timestamp` attribute on <testsuite> records the time the XML was
// produced — i.e. after the suite has finished — so it can't be used
// as the suite wall-clock origin for journal slicing.
//
// Missing or unparseable files are silently ignored: the analyzer
// falls back to the junit timestamp and the bundle still gets created,
// it just may have an empty (or off-window) journal slice.
func ApplySuiteStartFiles(rep *Report, logDir string) {
	if logDir == "" {
		return
	}
	for si := range rep.Suites {
		s := &rep.Suites[si]
		startPath := filepath.Join(logDir, fmt.Sprintf("test-%s.start", s.Label))
		data, err := os.ReadFile(startPath)
		if err != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: parse %s: %v\n", startPath, err)
			continue
		}
		if s.Root != nil {
			s.Root.StartAt = ts.Add(s.Root.OffsetFromSuiteStart)
		}
		for ci := range s.Cascades {
			s.Cascades[ci].StartAt = ts.Add(s.Cascades[ci].OffsetFromSuiteStart)
		}
		for bi := range s.Buckets {
			for fi := range s.Buckets[bi] {
				f := &s.Buckets[bi][fi]
				f.StartAt = ts.Add(f.OffsetFromSuiteStart)
			}
		}
	}
}

// WriteBundles materialises per-failure bundles under <logDir>/analysis/.
// Mutates rep in-place so each Failure.BundlePath holds the relative
// path the renderer will link from the summary.
//
// Failures with no parseable wall-clock start still get a bundle dir
// (and the testcase-window test.log slice), but no journal slice — the
// time window is unknown.
//
// Errors are non-fatal: a bundle that partially fails still gets linked
// from the report so a reader sees what's available, and the analyzer
// keeps printing warnings so CI logs surface the gap.
func WriteBundles(rep *Report, logDir string) {
	if logDir == "" {
		return
	}
	base := filepath.Join(logDir, "analysis")
	if err := os.MkdirAll(base, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: mkdir %s: %v\n", base, err)
		return
	}

	for si := range rep.Suites {
		s := &rep.Suites[si]
		if s.Root != nil {
			writeOneBundle(logDir, base, s.Label, s.Root)
		}
		for ci := range s.Cascades {
			writeOneBundle(logDir, base, s.Label, &s.Cascades[ci])
		}
		// Bucket entries share storage with Cascades but live in a
		// separate slice in SuiteReport; mirror the bundle path so the
		// renderer's bucket loop sees it too.
		for bi := range s.Buckets {
			for fi := range s.Buckets[bi] {
				f := &s.Buckets[bi][fi]
				if bp := findBundlePath(s, f.Name); bp != "" {
					f.BundlePath = bp
				}
			}
		}
	}
}

// findBundlePath looks up a Failure's already-written BundlePath by test
// name within a suite. Buckets reference the same logical failure as
// Cascades but are independent slice values, so we copy the path across
// rather than re-deriving it.
func findBundlePath(s *SuiteReport, testName string) string {
	if s.Root != nil && s.Root.Name == testName {
		return s.Root.BundlePath
	}
	for _, c := range s.Cascades {
		if c.Name == testName {
			return c.BundlePath
		}
	}
	return ""
}

func writeOneBundle(logDir, base, suiteLabel string, f *Failure) {
	name := fmt.Sprintf("%02d-%s", f.Order+1, sanitiseTestName(f.Name))
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: mkdir %s: %v\n", dir, err)
		return
	}
	f.BundlePath = filepath.Join("analysis", name)

	writeFailureSummary(dir, suiteLabel, f)

	if !f.StartAt.IsZero() {
		start := f.StartAt.Add(-bundlePad)
		end := f.StartAt.Add(time.Duration(f.Duration * float64(time.Second))).Add(bundlePad)
		journalSrc := filepath.Join(logDir, "spinifex-journal.log")
		if _, err := SliceJournal(journalSrc, start, end, filepath.Join(dir, "journal.log")); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: slice journal for %s: %v\n", f.Name, err)
		}
	}

	testLogSrc := filepath.Join(logDir, fmt.Sprintf("test-%s.log", suiteLabel))
	if _, err := SliceTestLog(testLogSrc, f.Name, filepath.Join(dir, "test.log")); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: slice test log for %s: %v\n", f.Name, err)
	}

	// Per-test artifact subdir written by harness.ArtifactDir lives at
	// <logDir>/<TestName with / replaced by _>/. Copy any files into
	// the bundle so qemu/ovs/etc. dumps already collected by the test
	// travel alongside the analysis.
	perTestSrc := filepath.Join(logDir, strings.ReplaceAll(f.Name, "/", "_"))
	if entries, err := os.ReadDir(perTestSrc); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			_ = copyFile(filepath.Join(perTestSrc, e.Name()), filepath.Join(dir, e.Name()))
		}
	} else if !os.IsNotExist(err) && !errorsIsNotDir(err) {
		fmt.Fprintf(os.Stderr, "::warning::e2e-analyze: read per-test dir %s: %v\n", perTestSrc, err)
	}
}

// errorsIsNotDir handles the case where the harness wrote a file at the
// per-test name instead of a directory — best-effort skip.
func errorsIsNotDir(err error) bool {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return false
	}
	return pe.Err != nil && strings.Contains(pe.Err.Error(), "not a directory")
}

func writeFailureSummary(dir, suiteLabel string, f *Failure) {
	var b strings.Builder
	fmt.Fprintf(&b, "Suite:    %s\n", suiteLabel)
	fmt.Fprintf(&b, "Test:     %s\n", f.Name)
	if !f.StartAt.IsZero() {
		fmt.Fprintf(&b, "Start:    %s\n", f.StartAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "Duration: %.3fs\n", f.Duration)
	if f.FileHint != "" {
		fmt.Fprintf(&b, "Site:     %s\n", f.FileHint)
	}
	if f.Cascade {
		b.WriteString("Cascade:  yes (downstream of another failure)\n")
	}
	if f.Error != "" {
		b.WriteString("\nError:\n")
		b.WriteString(oneLine(f.Error))
		b.WriteString("\n")
	}
	_ = os.WriteFile(filepath.Join(dir, "failure.txt"), []byte(b.String()), 0o644)
}
