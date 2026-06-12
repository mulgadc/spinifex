//go:build e2e

package harness

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// ANSI color codes. Disabled when stdout is not a TTY and GITHUB_ACTIONS is unset.
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

// Phase writes a colored section banner to stdout, bypassing t.Logf to avoid
// the framework's indentation. Use one line per logical phase, not per attribute.
func Phase(t *testing.T, format string, args ...any) {
	t.Helper()
	fmt.Fprintf(os.Stdout, "\n%s%s━━ %s ━━%s\n", colorBold, colorCyan, fmt.Sprintf(format, args...), colorReset)
}

// Step emits a sub-phase marker directly to stdout so it streams live in CI.
// Bypasses t.Logf, which buffers until PASS/FAIL and goes silent for long subtests.
func Step(t *testing.T, format string, args ...any) {
	t.Helper()
	fmt.Fprintf(os.Stdout, "%s· %s%s\n", colorDim, fmt.Sprintf(format, args...), colorReset)
}

// Detail emits a key=value line. Same direct-stdout rationale as Step.
// Pass alternating key, value, key, value …; mismatched lengths panic so
// the bug surfaces at the test boundary.
func Detail(t *testing.T, kvs ...any) {
	t.Helper()
	if len(kvs)%2 != 0 {
		panic("harness.Detail: odd number of arguments; want key, value pairs")
	}
	var parts []string
	for i := 0; i < len(kvs); i += 2 {
		parts = append(parts, fmt.Sprintf("%s%v%s=%v", colorDim, kvs[i], colorReset, kvs[i+1]))
	}
	fmt.Fprintln(os.Stdout, strings.Join(parts, " "))
}
