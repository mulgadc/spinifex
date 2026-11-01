package worker

import (
	"slices"
	"testing"

	sqsv1 "github.com/mulgadc/spinifex/contracts/sqs/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerrole"
)

// The registration dispatches exactly the actions the worker and store roles
// are granted, and the store's are exactly the ones gated to the store role.
func TestActionNames_MatchTheRoleGrant(t *testing.T) {
	want := slices.Sorted(slices.Values(append(workerrole.Actions(), sqsv1.Actions()...)))
	if got := ActionNames(); !slices.Equal(got, want) {
		t.Fatalf("dispatched actions %v, role grants %v", got, want)
	}
	for _, a := range sqsv1.Actions() {
		if !storeActions[a] || slices.Contains(workerrole.Actions(), a) {
			t.Fatalf("store action %s is not gated to the store role alone", a)
		}
	}
	if len(storeActions) != len(sqsv1.Actions()) {
		t.Fatalf("store-gated actions %v, store role grants %v", storeActions, sqsv1.Actions())
	}
}
