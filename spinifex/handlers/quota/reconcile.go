package handlers_quota

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// AccountLister enumerates the account IDs whose vCPU counters reconcile should
// recompute. It is satisfied by the gateway's active-account enumerator, and is
// what tells a whole-set pass about an account holding nothing: an account with
// no instances is absent from the scan, so only this list can zero it.
type AccountLister func() ([]string, error)

// Reconcile recomputes every account's vCPU counter from the live running-plus-
// stopped instance set and CAS-overwrites it. It is the only path that lowers a
// counter: it corrects the drift an out-of-band termination or a retype leaves
// behind and zeroes accounts that now hold nothing. The system account is exempt
// and never charged. A per-account write failure is logged and the pass
// continues; the first such error is returned so the caller can surface it.
//
// A counter is only lowered from a view good enough to lower it: the scan
// succeeded, and the counter has not moved since the revision read before the
// scan started. Otherwise the pass may raise but never lower, so an under-count
// cannot silently lift the cap.
func (s *Service) Reconcile(ctx context.Context, accounts AccountLister, list InstanceVCPULister) error {
	if s == nil || !s.limits.Enabled {
		return nil
	}
	ids, err := accounts()
	if err != nil {
		return fmt.Errorf("quota reconcile: list accounts: %w", err)
	}
	charged := make([]string, 0, len(ids))
	for _, accountID := range ids {
		if accountID != utils.GlobalAccountID {
			charged = append(charged, accountID)
		}
	}

	// Revisions come before the scan, not after: a charge landing while the
	// scan runs moves the revision, and the write then loses to the charge
	// rather than overwriting a launch the scan never saw.
	before := s.counterRevisions(ctx, charged)
	totals, complete, err := list(ctx)
	if err != nil {
		slog.Warn("quota reconcile: instance scan failed, counters left unchanged", "err", err)
		return err
	}

	var firstErr error
	for _, accountID := range charged {
		if err := ctx.Err(); err != nil {
			return err
		}
		// An account absent from the scan holds nothing and is zeroed, which
		// is the drift an out-of-band termination leaves behind.
		if err := s.reconcileVCPU(ctx, accountID, totals[accountID], complete, before[accountID]); err != nil {
			slog.Warn("quota reconcile: counter overwrite failed", "account", accountID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ReconcileAccount recomputes one account's counter. It is the per-key entry
// point: a change to one instance costs that account's recompute rather than a
// sweep of every account. The scan still reads the whole record space, because
// nothing indexes it by account, but it runs once per settled burst instead of
// once per account per tick.
func (s *Service) ReconcileAccount(ctx context.Context, accountID string, list InstanceVCPULister) error {
	if s == nil || !s.limits.Enabled || accountID == utils.GlobalAccountID {
		return nil
	}
	before := s.counterRevisions(ctx, []string{accountID})
	totals, complete, err := list(ctx)
	if err != nil {
		return fmt.Errorf("quota reconcile %s: instance scan: %w", accountID, err)
	}
	if err := s.reconcileVCPU(ctx, accountID, totals[accountID], complete, before[accountID]); err != nil {
		return fmt.Errorf("quota reconcile %s: counter overwrite: %w", accountID, err)
	}
	return nil
}

// counterRevisions reads the current revision of each account's counter. A
// read that fails reports revision zero, which reads as "no counter yet" and
// makes the later write a create that a concurrent charge wins.
func (s *Service) counterRevisions(ctx context.Context, accountIDs []string) map[string]uint64 {
	out := make(map[string]uint64, len(accountIDs))
	for _, accountID := range accountIDs {
		_, revision, err := s.readVCPU(ctx, accountID)
		if err != nil {
			slog.Warn("quota reconcile: counter revision read failed", "account", accountID, "err", err)
			continue
		}
		out[accountID] = revision
	}
	return out
}

// sumReservationVCPUs totals the catalog vCPUs of every non-terminal instance
// across the reservations. Terminated and shutting-down instances no longer
// exist for quota and are skipped; an unknown instance type contributes nothing.
func sumReservationVCPUs(reservations []*ec2.Reservation) int {
	total := 0
	for _, res := range reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst == nil || inst.InstanceType == nil || isTerminalState(inst.State) {
				continue
			}
			if vcpus, ok := instancetypes.DefaultVCPUs(*inst.InstanceType); ok {
				total += vcpus
			}
		}
	}
	return total
}

// isTerminalState reports whether an instance has left the counted set. Pending,
// running, stopping, and stopped all "exist" and are charged; only shutting-down
// and terminated are excluded so a reaped instance frees quota on the next pass.
func isTerminalState(state *ec2.InstanceState) bool {
	if state == nil || state.Name == nil {
		return false
	}
	switch *state.Name {
	case ec2.InstanceStateNameShuttingDown, ec2.InstanceStateNameTerminated:
		return true
	default:
		return false
	}
}
