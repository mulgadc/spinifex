//go:build e2e

package multinode

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// needAZ is a package-local shorthand for the discovered default AZ.
// Memoized on the harness fixture so every Test* gets the same answer
// regardless of execution order.
func needAZ(t *testing.T, fix *Fixture) string {
	t.Helper()
	return harness.DiscoverDefaultAZ(t, fix.Harness)
}

// needInstanceTypeArch returns the discovered nano instance type and its
// architecture. Memoized on the harness fixture.
func needInstanceTypeArch(t *testing.T, fix *Fixture) (instanceType, arch string) {
	t.Helper()
	return harness.DiscoverNanoInstanceType(t, fix.Harness)
}

// needAMI returns the discovered Ubuntu AMI for the given architecture.
// Memoized on the harness fixture.
func needAMI(t *testing.T, fix *Fixture, arch string) string {
	t.Helper()
	return harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
}

// readyNodeCount counts the number of "Ready" lines in `spx get nodes`
// output. Bash phase 2 used `grep -c "Ready"`; we match the same
// (substring, case-sensitive) so cluster-status string drift surfaces in
// both tracks identically.
func readyNodeCount(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Ready") {
			n++
		}
	}
	return n
}
