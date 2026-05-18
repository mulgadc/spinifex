// Package main implements the e2e-analyze GitHub Action.
//
// Stage 1 of docs/development/improvements/e2e-go-failure-analysis.md:
// cluster failures from go-junit-report's junit-*.xml by error signature,
// pick the earliest non-cascade failure per suite as the likely root cause,
// and render a human-readable report.
//
// This file contains the pure logic (parsing, signature extraction,
// clustering, rendering). Side-effect handling lives in main.go.
package main

import (
	"encoding/xml"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// JUnit XML schema produced by go-junit-report v2. Stable across versions
// we care about; only fields used downstream are decoded.
type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string    `xml:"name,attr"`
	Tests     int       `xml:"tests,attr"`
	Failures  int       `xml:"failures,attr"`
	Errors    int       `xml:"errors,attr"`
	Skipped   int       `xml:"skipped,attr"`
	Timestamp string    `xml:"timestamp,attr"`
	Time      float64   `xml:"time,attr"`
	Cases     []junitTC `xml:"testcase"`
}

type junitTC struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure"`
	Error     *junitFailure `xml:"error"`
	Skipped   *junitSkipped `xml:"skipped"`
	SystemOut string        `xml:"system-out"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

// Failure is the analyzer's normalized view of one failed testcase.
type Failure struct {
	SuiteFile string    // e.g. "single" (derived from junit-single.xml)
	Name      string    // e.g. "TestSingleNode/Phase5_LaunchInstance"
	Duration  float64   // seconds
	Order     int       // position within the suite (0-based)
	StartAt   time.Time // estimated wall-clock start: suite.timestamp + sum(prior durations)
	Error     string    // last meaningful error line, ANSI-stripped
	FileHint  string    // e.g. "vpc_test.go:227", parsed from failure body
	Cascade   bool      // signature matched a known downstream pattern
	Signature string    // normalized error key for bucketing
}

// SuiteReport summarises one junit-*.xml file.
type SuiteReport struct {
	File      string // basename, e.g. "junit-single.xml"
	Label     string // short name, e.g. "single"
	Total     int
	FailCount int
	Root      *Failure    // nil if no failures
	Cascades  []Failure   // everything else that failed in this suite
	Buckets   [][]Failure // failures grouped by signature (root's bucket first)
}

// Report is the full analysis across every junit file.
type Report struct {
	Title  string
	Suites []SuiteReport
}

// ansiSeq matches ANSI CSI escapes the harness sprinkles through log
// output (colours, bold). Three forms in the wild:
//   - real ESC: `\x1b[36m…`
//   - U+FFFD replacement (XML serialiser dropped the ESC byte): `�[36m…`
//   - bare CSI with the leading byte gone entirely: `[36m…`
//
// All three are stripped so colour state doesn't fork the signature bucket.
var ansiSeq = regexp.MustCompile(
	"\x1b\\[[0-9;]*[a-zA-Z]" + // real ESC CSI
		"|�\\[[0-9;]*[a-zA-Z]" + // replacement-char CSI
		"|\\[[0-9]{1,2}(?:;[0-9]{1,2})*m", // bare colour code, "mode" form only
)

// cascadeMarkers are message fragments that only appear in tests that
// depend on a fixture field populated by an earlier phase. Their presence
// means the failure is downstream of some other failure that happened
// first; the report collapses them under that root cause.
var cascadeMarkers = []string{
	"must populate fix.",
	"Should NOT be empty",
	"Expected value not to be nil",
}

// noiseReplacers normalise dynamic identifiers so that "ten failures, same
// bug" bucket together. Pattern ordering matters: longer/more-specific
// patterns must precede the catch-alls.
var noiseReplacers = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\b(vpc|subnet|sg|rtb|igw|i|ami|vol|snap|rtbassoc|key)-[0-9a-f]{8,}\b`), `${1}-<id>`},
	{regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), `<uuid>`},
	{regexp.MustCompile(`\b[0-9a-f]{32,}\b`), `<hex>`},
	{regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`), `<ip>`},
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`), `<ts>`},
	{regexp.MustCompile(`request-id:\s*\S+`), `request-id: <id>`},
	{regexp.MustCompile(`(_test\.go):\d+`), `${1}:<line>`},
	{regexp.MustCompile(`\s+`), ` `},
}

// fileHintRe pulls the last "foo_test.go:NNN" we see in a failure body so
// the report can point a reader at the source line. Test files dominate the
// hint set; harness/helper file matches go through goFileLineRe below.
var fileHintRe = regexp.MustCompile(`([A-Za-z0-9_]+_test\.go:\d+)`)

// goFileLineRe matches any "foo.go:NNN:" prefix (test or non-test). Used to
// surface harness/helper-level error text like
// `ec2helpers.go:164: describe-vpcs: InvalidParameterValue …` which Go test
// emits when a non-test file calls t.Errorf.
var goFileLineRe = regexp.MustCompile(`^[A-Za-z0-9_]+\.go:\d+:\s*(.+)$`)

// testifyErrorPlaceholder is testify's "Error:" boilerplate when the
// assertion failed because of a wrapped error. The next non-blank line in
// the trace holds the actual error text.
const testifyErrorPlaceholder = "Received unexpected error:"

// stripANSI removes CSI escapes; leaves everything else intact.
func stripANSI(s string) string {
	return ansiSeq.ReplaceAllString(s, "")
}

// extractErrorLine finds the most useful single-line summary in a failure
// body. Priority order, highest first:
//  1. Testify "Messages:" line — user-supplied context, the most signal-rich.
//  2. Testify "Error:" content (with one-line lookahead when the placeholder
//     "Received unexpected error:" defers to the next line).
//  3. Leading "foo.go:NNN: text" content line — t.Errorf from non-test code.
//  4. Last `_test.go:NNN: text` line — bare t.Fatalf/Errorf from a test file.
//  5. First non-empty line of the body.
func extractErrorLine(body string) string {
	body = stripANSI(body)
	lines := strings.Split(body, "\n")

	var (
		msgLine      string
		errLine      string
		goFileLine   string
		lastTestLine string
	)
	for i, raw := range lines {
		l := strings.TrimSpace(raw)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "Messages:") {
			msgLine = strings.TrimSpace(strings.TrimPrefix(l, "Messages:"))
			continue
		}
		if strings.HasPrefix(l, "Error:") && errLine == "" {
			content := strings.TrimSpace(strings.TrimPrefix(l, "Error:"))
			if content == testifyErrorPlaceholder {
				// Look ahead for the wrapped error text on the next non-blank line.
				for j := i + 1; j < len(lines); j++ {
					nxt := strings.TrimSpace(lines[j])
					if nxt == "" {
						continue
					}
					if strings.HasPrefix(nxt, "Test:") || strings.HasPrefix(nxt, "Messages:") {
						break
					}
					content = content + " " + nxt
					break
				}
			}
			errLine = content
			continue
		}
		if m := goFileLineRe.FindStringSubmatch(l); m != nil {
			// Keep the LAST occurrence: t.Log / harness traces emit
			// progress lines from the same file:line pattern before the
			// failing assertion finally fires.
			goFileLine = strings.TrimSpace(m[1])
			continue
		}
		if fileHintRe.MatchString(l) {
			lastTestLine = l
		}
	}
	switch {
	case msgLine != "":
		return msgLine
	case errLine != "":
		return errLine
	case goFileLine != "":
		return goFileLine
	case lastTestLine != "":
		// "vpc_test.go:227: Eventually: condition not met …" → trim the file prefix.
		if i := strings.Index(lastTestLine, ": "); i > 0 {
			return strings.TrimSpace(lastTestLine[i+2:])
		}
		return lastTestLine
	}
	// Last resort: first non-empty line of the body.
	for _, raw := range lines {
		l := strings.TrimSpace(raw)
		if l != "" {
			return l
		}
	}
	return ""
}

// errorTraceRe extracts the path from testify's "Error Trace:" line — e.g.
//
//	Error Trace:	tests/e2e/harness/ec2helpers.go:164
//
// — which points at the assertion site even when it lives in helper code
// outside *_test.go.
var errorTraceRe = regexp.MustCompile(`Error Trace:\s*(\S+\.go:\d+)`)

// extractFileHint returns the most precise pointer at the assertion site
// we can pull from the failure body: testify Error Trace wins, otherwise
// the last `*_test.go:NNN` line in the body.
func extractFileHint(body string) string {
	body = stripANSI(body)
	if m := errorTraceRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	all := fileHintRe.FindAllString(body, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

// signature normalises an error line so different runs of the same root
// cause bucket together. Empty signatures (e.g. parent-test placeholders
// where the failure body is empty) bucket together under "".
func signature(errLine string) string {
	s := strings.ToLower(errLine)
	for _, r := range noiseReplacers {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return strings.TrimSpace(s)
}

// isCascade reports whether the error line contains any of the well-known
// fixture-propagation markers — i.e. "this test failed because something
// upstream of it failed first".
func isCascade(errLine string) bool {
	for _, m := range cascadeMarkers {
		if strings.Contains(errLine, m) {
			return true
		}
	}
	return false
}

// parseStartTime turns a junit timestamp attr (RFC3339) into a time.Time;
// zero value if unparseable.
func parseStartTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// suiteLabel turns "junit-single.xml" into "single".
func suiteLabel(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".xml")
	base = strings.TrimPrefix(base, "junit-")
	if base == "" {
		return filepath.Base(path)
	}
	return base
}

// ParseFile decodes one junit-*.xml file into our normalized failure list.
// Returns the suite label, total testcase count, and the (ordered) failures.
func ParseFile(path string, data []byte) (SuiteReport, error) {
	var doc junitSuites
	if err := xml.Unmarshal(data, &doc); err != nil {
		return SuiteReport{}, fmt.Errorf("parse %s: %w", path, err)
	}

	rep := SuiteReport{
		File:  filepath.Base(path),
		Label: suiteLabel(path),
	}

	order := 0
	for _, suite := range doc.Suites {
		start := parseStartTime(suite.Timestamp)
		cumul := 0.0
		for _, tc := range suite.Cases {
			rep.Total++
			var body string
			switch {
			case tc.Failure != nil:
				body = tc.Failure.Body
			case tc.Error != nil:
				body = tc.Error.Body
			default:
				cumul += tc.Time
				continue
			}

			// Skip the synthetic parent-test "Failed" stub that go-junit-report
			// emits for any Go test whose subtests failed. It has an empty body
			// and would otherwise outrank real failures by XML order.
			trimmed := strings.TrimSpace(stripANSI(body))
			if trimmed == "" {
				cumul += tc.Time
				continue
			}

			errLine := extractErrorLine(body)
			f := Failure{
				SuiteFile: rep.Label,
				Name:      tc.Name,
				Duration:  tc.Time,
				Order:     order,
				Error:     errLine,
				FileHint:  extractFileHint(body),
				Cascade:   isCascade(errLine),
				Signature: signature(errLine),
			}
			if !start.IsZero() {
				f.StartAt = start.Add(time.Duration(cumul * float64(time.Second)))
			}
			rep.FailCount++
			rep.Cascades = append(rep.Cascades, f) // temp staging; split below
			order++
			cumul += tc.Time
		}
	}

	if rep.FailCount == 0 {
		rep.Cascades = nil
		return rep, nil
	}

	all := rep.Cascades
	rep.Cascades = nil

	// Root = earliest non-cascade. If every failure is a cascade (the whole
	// suite tripped on the same upstream gap), fall back to earliest overall.
	rootIdx := -1
	for i, f := range all {
		if !f.Cascade {
			rootIdx = i
			break
		}
	}
	if rootIdx == -1 {
		rootIdx = 0
	}
	root := all[rootIdx]
	rep.Root = &root

	// Bucket the remainder by signature so the report can collapse the
	// "ten failures, same bug" pattern.
	bucketKeys := []string{}
	buckets := map[string][]Failure{}
	for i, f := range all {
		if i == rootIdx {
			continue
		}
		if _, seen := buckets[f.Signature]; !seen {
			bucketKeys = append(bucketKeys, f.Signature)
		}
		buckets[f.Signature] = append(buckets[f.Signature], f)
		rep.Cascades = append(rep.Cascades, f)
	}

	// Largest bucket first; ties broken by first-seen order so output is
	// deterministic across runs.
	sort.SliceStable(bucketKeys, func(i, j int) bool {
		return len(buckets[bucketKeys[i]]) > len(buckets[bucketKeys[j]])
	})
	for _, k := range bucketKeys {
		rep.Buckets = append(rep.Buckets, buckets[k])
	}

	return rep, nil
}

// Render writes the markdown report. Format is documented in
// docs/development/improvements/e2e-go-failure-analysis.md (Stage 1).
func Render(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", r.Title)

	totalFail := 0
	for _, s := range r.Suites {
		totalFail += s.FailCount
	}
	if totalFail == 0 {
		b.WriteString("✅ No failures across any suite.\n")
		return b.String()
	}

	for _, s := range r.Suites {
		if s.FailCount == 0 {
			fmt.Fprintf(&b, "### Suite `%s`: ✅ pass (%d tests)\n\n", s.Label, s.Total)
			continue
		}
		fmt.Fprintf(&b, "### Suite `%s`: %d failed, 1 root cause likely\n\n", s.Label, s.FailCount)

		if s.Root != nil {
			b.WriteString("**Root cause (earliest non-cascade)**\n\n")
			fmt.Fprintf(&b, "- Test: `%s`\n", s.Root.Name)
			if !s.Root.StartAt.IsZero() {
				fmt.Fprintf(&b, "- Start: %s (duration %.1fs)\n",
					s.Root.StartAt.Format("15:04:05"), s.Root.Duration)
			} else {
				fmt.Fprintf(&b, "- Duration: %.1fs\n", s.Root.Duration)
			}
			if s.Root.Error != "" {
				fmt.Fprintf(&b, "- Error: %s\n", oneLine(s.Root.Error))
			}
			if s.Root.FileHint != "" {
				fmt.Fprintf(&b, "- File:  `%s`\n", s.Root.FileHint)
			}
			b.WriteString("\n")
		}

		if len(s.Buckets) > 0 {
			fmt.Fprintf(&b, "**Cascaded failures (%d) — grouped by signature**\n\n",
				len(s.Cascades))
			for _, bucket := range s.Buckets {
				sample := bucket[0]
				label := oneLine(sample.Error)
				if label == "" {
					label = "(no error message)"
				}
				fmt.Fprintf(&b, "- %d× %s\n", len(bucket), label)
				// List up to 5 test names per bucket; collapse the rest.
				const cap = 5
				for i, f := range bucket {
					if i == cap && len(bucket) > cap {
						fmt.Fprintf(&b, "  - … and %d more\n", len(bucket)-cap)
						break
					}
					fmt.Fprintf(&b, "  - `%s`\n", f.Name)
				}
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

// oneLine collapses whitespace and caps length so a runaway assertion
// message doesn't blow out the summary card width.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	const max = 160
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}
