//test:in-package — validateNodeCount is an unexported guard on this command's
// flags, and exporting it would widen the package's API for a test's convenience.

package cmd

import (
	"strings"
	"testing"
)

// TestValidateNodeCount pins the supported cluster sizes. Two is rejected rather
// than warned about: on two servers no layer of the stack has a majority to lose,
// and the control plane's own state has no replica count that both survives a node
// going away and still accepts writes.
func TestValidateNodeCount(t *testing.T) {
	for _, nodes := range []int{1, 3, 4, 5, 9} {
		if err := validateNodeCount(nodes); err != nil {
			t.Errorf("validateNodeCount(%d) = %v, want nil", nodes, err)
		}
	}

	for _, nodes := range []int{0, -1} {
		if err := validateNodeCount(nodes); err == nil {
			t.Errorf("validateNodeCount(%d) returned no error", nodes)
		}
	}

	err := validateNodeCount(2)
	if err == nil {
		t.Fatal("validateNodeCount(2) returned no error")
	}
	// The message has to say what to do instead, because the operator typed a
	// number and needs to know which one to type next.
	for _, want := range []string{"2", "1", "3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
