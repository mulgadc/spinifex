package worker

import (
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerrole"
)

// The registration dispatches exactly the actions the worker role is granted.
func TestActionNames_MatchTheRoleGrant(t *testing.T) {
	if got, want := ActionNames(), workerrole.Actions(); !slices.Equal(got, want) {
		t.Fatalf("dispatched actions %v, role grants %v", got, want)
	}
}
