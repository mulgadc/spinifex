package ochre

import (
	"errors"
	"fmt"
	"math"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
)

// errEndpointWrite is a claim or hold that does not belong to its operation
// or changes more than it must.
var errEndpointWrite = errors.New("bedrock: not an allowed endpoint claim or hold")

var _ readiness.WriteCheck = CheckEndpointWrite

// CheckEndpointWrite allows a launch token to claim an empty key with a bare
// STARTING record it owns, and a sweep token to add its own hold to an unheld
// global READY record. Everything else is refused.
func CheckEndpointWrite(w readiness.EndpointWrite) error {
	var err error
	switch w.Intent {
	case readiness.IntentClaim:
		err = checkClaim(w)
	case readiness.IntentHold:
		err = checkHold(w)
	default:
		err = fmt.Errorf("intent %q is not a claim or hold", w.Intent)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errEndpointWrite, err)
	}
	return nil
}

// checkClaim allows a bare STARTING record, owned by this launch token's ID,
// epoch, nonce and node, on a key that is absent or carries a delete marker.
func checkClaim(w readiness.EndpointWrite) error {
	switch {
	case w.Kind != readiness.TokenLaunch || w.TokenID == "" || w.Epoch == 0 || w.Node == "" || w.Nonce == "":
		return errors.New("a claim needs an admitted launch token")
	case w.Target == "" || w.Target != w.Key:
		return fmt.Errorf("token target %q is not key %q", w.Target, w.Key)
	case w.Prior.Live():
		return errors.New("a claim needs a key with no value")
	}
	rec, err := strictRecord(w.Next)
	if err != nil {
		return fmt.Errorf("new record: %w", err)
	}
	if rec.AccountID == "" || resolveKey(rec.AccountID, rec.ModelID) != w.Key {
		return fmt.Errorf("record does not belong at key %q", w.Key)
	}
	if _, found, selfHost := LookupCoServeGroup(rec.ModelID); !found || !selfHost {
		return errors.New("model is not a self-hosted catalog entry")
	}
	owner, ok, err := rec.Owner()
	switch {
	case err != nil || !ok:
		return errors.New("a claim needs a valid launch owner")
	case owner.TokenID != w.TokenID || owner.Epoch != w.Epoch || owner.WorkerNonce != w.Nonce || rec.NodeID != w.Node:
		return errors.New("launch owner is not this operation")
	case rec.State != StateStarting || rec.Generation != 1 || !bareClaim(rec) || !unobserved(rec):
		return errors.New("a claim is a bare STARTING record at generation 1")
	case rec.ParkedReason != "" || !rec.ParkedAt.IsZero() || rec.HealthHold != nil || !rec.ReadyAt.IsZero():
		return errors.New("a claim carries no marker, hold or ready time")
	case rec.OperatorReserved && !rec.Pinned:
		return errors.New("an operator reservation needs a pin")
	}
	return nil
}

// checkHold allows the observed unheld global READY record with only this
// sweep's hold added and its generation advanced once. The bundle need not
// be verified: an endpoint of doubtful identity needs the hold too.
func checkHold(w readiness.EndpointWrite) error {
	switch {
	case w.Kind != readiness.TokenSweep || w.TokenID == "" || w.Epoch == 0 || w.Node == "":
		return errors.New("a hold needs an admitted sweep token")
	case w.Target != SweepTarget:
		return fmt.Errorf("token target %q is not the sweep scope", w.Target)
	case !w.Prior.Live():
		return errors.New("a hold needs a live record")
	}
	old, err := strictRecord(w.Prior.Value)
	if err != nil {
		return fmt.Errorf("stored record: %w", err)
	}
	next, err := strictRecord(w.Next)
	if err != nil {
		return fmt.Errorf("new record: %w", err)
	}
	switch {
	case resolveKey(resolveAccountID(old.AccountID), old.ModelID) != w.Key:
		return fmt.Errorf("record does not belong at key %q", w.Key)
	case globalReady(old) != nil:
		return errors.New("a sweep holds global endpoints only")
	case old.State != StateReady || old.HealthHold != nil:
		return errors.New("a hold needs an unheld READY record")
	case old.Generation == math.MaxUint64:
		return errors.New("generation cannot advance")
	case next.HealthHold == nil || next.HealthHold.ReservedAt.IsZero():
		return errors.New("a hold needs its reservation time")
	}
	want := old
	want.HealthHold = &HealthHold{Token: w.TokenID, Epoch: w.Epoch, ReservedAt: next.HealthHold.ReservedAt}
	want.Generation = old.Generation + 1
	same, err := sameEncoding(want, next)
	if err != nil {
		return err
	}
	if !same {
		return errors.New("new record changes more than the hold")
	}
	return nil
}
