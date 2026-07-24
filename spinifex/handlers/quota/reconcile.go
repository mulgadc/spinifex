package handlers_quota

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// AccountLister enumerates the account IDs whose vCPU counters reconcile should
// recompute. It is satisfied by the gateway's active-account enumerator, so a
// global describe (which does not exist) is never needed: each account is swept
// through its own account-filtered Describe* call.
type AccountLister func() ([]string, error)

// InstanceLister returns the reservations one account currently holds and whether
// the sweep was complete. The production implementation is the account-filtered
// DescribeInstances fan-out, which bundles running plus stopped plus terminated;
// reconcile drops the terminal set so only the "existing" (running plus stopped)
// vCPUs are summed. complete is false when a node or instance bucket did not
// answer, so reconcile must not lower a counter from that partial view.
type InstanceLister func(accountID string) (reservations []*ec2.Reservation, complete bool, err error)

// NATSInstanceLister builds the production InstanceLister from a NATS connection
// and the configured cluster node count. The count is the expected node total,
// not the live-active count, so a node that is down makes the sweep incomplete
// rather than silently dropping its instances; reconcile then leaves the counter
// untouched instead of lowering it. expectedNodes is re-evaluated per call so a
// config change between passes is reflected without a gateway restart.
func NATSInstanceLister(natsConn *nats.Conn, expectedNodes func() int) InstanceLister {
	return func(accountID string) ([]*ec2.Reservation, bool, error) {
		return gateway_ec2_instance.DescribeInstancesForReconcile(
			context.Background(), &ec2.DescribeInstancesInput{}, natsConn, expectedNodes(), accountID)
	}
}

// Reconcile recomputes every account's vCPU counter from the live running-plus-
// stopped instance set and CAS-overwrites it. It is the only path that lowers a
// counter: it corrects the drift an out-of-band termination or a retype leaves
// behind and zeroes accounts that now hold nothing. The system account is exempt
// and never charged. A per-account describe or write failure is logged and the
// pass continues; the first such error is returned so the caller can surface it.
// A counter is only lowered when the sweep was complete (every node and instance
// bucket answered); a partial sweep may raise but never lower, so a transient
// node outage cannot silently under-count usage and lift the cap.
func (s *Service) Reconcile(ctx context.Context, accounts AccountLister, list InstanceLister) error {
	if s == nil || !s.limits.Enabled {
		return nil
	}
	ids, err := accounts()
	if err != nil {
		return fmt.Errorf("quota reconcile: list accounts: %w", err)
	}
	var firstErr error
	for _, accountID := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if accountID == utils.GlobalAccountID {
			continue
		}
		reservations, complete, err := list(accountID)
		if err != nil {
			slog.Warn("quota reconcile: describe failed, counter left unchanged", "account", accountID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.reconcileVCPU(ctx, accountID, sumReservationVCPUs(reservations), complete); err != nil {
			slog.Warn("quota reconcile: counter overwrite failed", "account", accountID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
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
