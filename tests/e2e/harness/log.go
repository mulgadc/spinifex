//go:build e2e

package harness

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// ANSI color codes — rendered live by the Actions UI; raw escapes pass
// through to artifact log files harmlessly. Disabled when stdout is not
// a TTY and GITHUB_ACTIONS isn't set, so piped local `go test -v` stays
// clean.
var (
	colorBold  = "\x1b[1m"
	colorDim   = "\x1b[2m"
	colorCyan  = "\x1b[36m"
	colorReset = "\x1b[0m"
)

func init() {
	if os.Getenv("GITHUB_ACTIONS") == "" && os.Getenv("FORCE_COLOR") == "" {
		if fi, _ := os.Stdout.Stat(); fi == nil || (fi.Mode()&os.ModeCharDevice) == 0 {
			colorBold, colorDim, colorCyan, colorReset = "", "", "", ""
		}
	}
}

// Phase writes a colored section banner directly to stdout, bypassing
// t.Logf so the line lands flush-left without the testing framework's
// `<file>:<line>:` prefix or forced indent. Banners are pure visual
// structure — the diagnostic resource IDs themselves stay on t.Logf
// (see Detail) so they remain attributed to the owning test in JUnit XML.
//
// Use sparingly: one line per logical phase, not per attribute.
func Phase(t *testing.T, format string, args ...any) {
	t.Helper()
	fmt.Fprintf(os.Stdout, "\n%s%s━━ %s ━━%s\n", colorBold, colorCyan, fmt.Sprintf(format, args...), colorReset)
}

// Step emits a single-line sub-phase marker. Dual-emits to os.Stdout so
// long-running subtests stream progress live in CI (the testing framework
// buffers t.Logf output per-test and releases only on PASS/FAIL), and to
// t.Logf so JUnit XML still attributes the line to the owning test.
// Duplicate output in the failure case is acceptable — both copies carry
// the same content; the stdout copy gives a triager live signal, the
// t.Logf copy keeps the per-test bucketing intact.
func Step(t *testing.T, format string, args ...any) {
	t.Helper()
	line := fmt.Sprintf("%s· %s%s", colorDim, fmt.Sprintf(format, args...), colorReset)
	fmt.Fprintln(os.Stdout, line)
	t.Logf("%s", line)
}

// Detail emits a key=value line. Same dual-emit rationale as Step. Pass
// alternating key, value, key, value …; mismatched lengths panic so the
// bug surfaces at the test boundary.
func Detail(t *testing.T, kvs ...any) {
	t.Helper()
	if len(kvs)%2 != 0 {
		panic("harness.Detail: odd number of arguments; want key, value pairs")
	}
	var parts []string
	for i := 0; i < len(kvs); i += 2 {
		parts = append(parts, fmt.Sprintf("%s%v%s=%v", colorDim, kvs[i], colorReset, kvs[i+1]))
	}
	line := strings.Join(parts, " ")
	fmt.Fprintln(os.Stdout, line)
	t.Logf("%s", line)
}
