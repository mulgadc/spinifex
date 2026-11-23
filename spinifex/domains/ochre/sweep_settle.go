package ochre

import (
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

// SweepTarget is the target of a reaper sweep token: one token covers every
// endpoint key the sweep settles.
const SweepTarget = "endpoints/sweep"

// errSweepSettlement is a sweep rewrite that is not a recovery settlement, or
// whose record is outside the sweep's scope.
var errSweepSettlement = errors.New("bedrock: not a sweep settlement of an endpoint record")

var _ readiness.SettleCheck = CheckSweepSettlement

// CheckSweepSettlement allows a sweep token to clear the hold of a held global
// READY record, or to promote a parked complete bundle in any account, at a
// revision it may not have written. It cannot show that the probe ran.
func CheckSweepSettlement(s readiness.Settlement) error {
	switch {
	case s.Kind != readiness.TokenSweep || s.TokenID == "" || s.Epoch == 0 || s.Node == "":
		return fmt.Errorf("%w: not an admitted sweep token", errSweepSettlement)
	case s.Next == nil:
		return fmt.Errorf("%w: a sweep never removes a record", errSweepSettlement)
	case s.Target != SweepTarget:
		return fmt.Errorf("%w: token target %q is not the sweep scope", errSweepSettlement, s.Target)
	}
	if err := checkRecovery(s.Key, s.Old, s.Next, globalReady); err != nil {
		return fmt.Errorf("%w: %w", errSweepSettlement, err)
	}
	return nil
}

// globalReady limits a sweep's hold clear to the shared global endpoints the
// reaper probes; an account-scoped READY record is outside its scope.
func globalReady(rec EndpointRecord) error {
	if resolveAccountID(rec.AccountID) != awsidentifiers.GlobalAccountID {
		return errors.New("a sweep clears holds on global endpoints only")
	}
	return nil
}
