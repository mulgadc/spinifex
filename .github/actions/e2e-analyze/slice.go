package main

// Stage 2 of docs/development/improvements/e2e-go-failure-analysis.md:
// per-failure log slicers used by bundle.go to materialise time-window /
// testcase-window log excerpts next to each named failure.

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// shortIsoTSRe matches the timestamp prefix produced by
// `journalctl --output=short-iso`. The offset format varies across
// systemd versions: most ship "+10:00" (RFC3339), some ship "+1000"
// (no colon). Match either — the parser below picks the right layout.
var shortIsoTSRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:[+-]\d{2}:?\d{2}|Z))\s`)

// shortIsoLayouts are tried in order; the first parse that succeeds wins.
var shortIsoLayouts = []string{
	time.RFC3339,               // 2026-05-19T12:32:30+10:00
	"2006-01-02T15:04:05-0700", // 2026-05-19T12:32:30+1000
}

func parseShortIso(s string) (time.Time, error) {
	var lastErr error
	for _, layout := range shortIsoLayouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return time.Time{}, lastErr
}

// SliceJournal copies lines from src whose `short-iso` timestamp falls in
// [start, end] to dst. Lines without a parseable timestamp inherit the
// previous line's classification — that keeps multi-line journal records
// (stack traces, panics) intact rather than orphaning their continuation
// lines. Lines that precede the first parseable timestamp are dropped.
//
// Returns the number of lines written; missing src is not an error (the
// journal may be empty if collection failed).
func SliceJournal(src string, start, end time.Time, dst string) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	bw := bufio.NewWriter(out)
	defer bw.Flush()

	sc := bufio.NewScanner(f)
	// Journal lines can be long (panic dumps); bump scanner buffer.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	keep := false
	written := 0
	for sc.Scan() {
		line := sc.Text()
		if m := shortIsoTSRe.FindStringSubmatch(line); m != nil {
			ts, err := parseShortIso(m[1])
			if err != nil {
				continue
			}
			keep = !ts.Before(start) && !ts.After(end)
		}
		if keep {
			if _, err := bw.WriteString(line); err != nil {
				return written, err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return written, err
			}
			written++
		}
	}
	return written, sc.Err()
}

// runRePrefix and finishRePrefix bracket a single testcase's output in
// `go test -v` stdout. Subtests have their own `=== RUN` / `--- FAIL`
// markers, so slicing on the exact testcase name yields just that test's
// lines (including any nested phase logs).
var (
	runRe    = regexp.MustCompile(`^=== RUN\s+(\S+)$`)
	finishRe = regexp.MustCompile(`^\s*---\s+(PASS|FAIL|SKIP):\s+(\S+)\s+\(`)
)

// SliceTestLog copies lines belonging to the named testcase from src to
// dst. The block starts at `=== RUN <name>` and ends at the matching
// `--- PASS|FAIL|SKIP: <name> (…)` line (inclusive). If the testcase is
// never seen the destination file is created empty so the bundle has a
// stable shape.
//
// Returns the number of lines written; missing src is not an error.
func SliceTestLog(src, testName, dst string) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	bw := bufio.NewWriter(out)
	defer bw.Flush()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	in := false
	written := 0
	for sc.Scan() {
		line := sc.Text()
		if !in {
			if m := runRe.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil && m[1] == testName {
				in = true
			} else {
				continue
			}
		}
		if _, err := bw.WriteString(line); err != nil {
			return written, err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return written, err
		}
		written++
		if m := finishRe.FindStringSubmatch(line); m != nil && m[2] == testName {
			return written, nil
		}
	}
	return written, sc.Err()
}

// copyFile is a small helper used when bundling pre-collected per-test
// artifacts. Best-effort: missing src is silently skipped.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
