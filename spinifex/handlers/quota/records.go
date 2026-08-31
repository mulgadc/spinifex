package handlers_quota

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// InstanceVCPULister totals the vCPUs each account currently holds, in one
// read of the whole instance record space rather than one describe per account.
//
// An account with no instances is absent from the map rather than present as
// zero: the caller knows which accounts exist and reads a missing one as zero,
// which is what zeroes an account that has just released everything.
//
// complete reports whether the view is good enough to lower a counter. It is
// false when the read failed and when the read cannot prove it was current, so
// a counter may still be raised from whatever was seen but never lowered from
// a view known to be short or suspected of being stale.
type InstanceVCPULister func(ctx context.Context) (perAccount map[string]int, complete bool, err error)

// recordSource is the pair of reads the sweep needs: the stream position it
// must reach, and the snapshot it judges against that position. It exists so a
// test can hand the sweep a snapshot that is deliberately behind, which a live
// single-node server never produces.
type recordSource interface {
	LastSequence(ctx context.Context, filter string) (uint64, error)
	Snapshot(ctx context.Context, filter string) ([]kvstore.Item[vm.InstanceRecord], uint64, error)
}

var _ recordSource = (*kvstore.Store[vm.InstanceRecord])(nil)

// RecordVCPULister totals vCPUs from the per-resource instance record space.
// The account is on the record and vCPUs derive from the instance type, which
// is spec, so the whole sweep is one KV read where it used to be a NATS
// fan-out of describes costing passes x accounts x (nodes + 2).
//
// There is no index by account, so a recompute of one account still reads the
// whole prefix. That is one read per settled burst, which the debounce already
// coalesces, against a fan-out that ran per account on every tick.
//
// The watermark is taken before the snapshot, not after. Every record write
// that had completed when the pass began sits at or below it, so a snapshot
// reaching it has seen all of them and may lower a counter. A write landing
// after it belongs to the pass its own watch update triggers.
func RecordVCPULister(records recordSource, prefix string) InstanceVCPULister {
	filter := prefix + "*"
	return func(ctx context.Context) (map[string]int, bool, error) {
		watermark, watermarkErr := records.LastSequence(ctx, filter)
		items, highWater, err := records.Snapshot(ctx, filter)
		if err != nil {
			return nil, false, err
		}

		totals := make(map[string]int, len(items))
		for i := range items {
			record := &items[i].Value
			accountID := record.Metadata.AccountID
			if accountID == "" || accountID == utils.GlobalAccountID {
				continue
			}
			if recordIsTerminal(record) {
				continue
			}
			// An unknown instance type contributes nothing, matching the sum
			// the fan-out produced.
			if vcpus, ok := instancetypes.DefaultVCPUs(record.Spec.InstanceType); ok {
				totals[accountID] += vcpus
			}
		}

		// A snapshot short of the watermark was served by a replica that has
		// not caught up, and lowering from it would undo a charge for an
		// instance it never saw. Raising is still safe and still happens.
		if watermarkErr != nil {
			slog.WarnContext(ctx, "quota: instance watermark unavailable, counters may not be lowered",
				"err", watermarkErr)
			return totals, false, nil
		}
		if highWater < watermark {
			slog.WarnContext(ctx, "quota: instance snapshot behind the stream, counters may not be lowered",
				"high_water", highWater, "watermark", watermark)
			return totals, false, nil
		}
		return totals, true, nil
	}
}

// recordIsTerminal reports whether a record has left the counted set. Pending,
// running, stopping and stopped all exist and are charged; only shutting-down
// and terminated are excluded, so a reaped instance frees quota. This mirrors
// isTerminalState, which reads the same states off the AWS-shaped projection.
func recordIsTerminal(record *vm.InstanceRecord) bool {
	switch record.Status.Status {
	case vm.StateShuttingDown, vm.StateTerminated:
		return true
	default:
		return false
	}
}

// AccountForRecord returns the account a raw instance record belongs to, for
// mapping a watch update onto the unit of work quota recomputes. ok is false
// when the value does not decode or carries no account, which the caller reads
// as "cannot attribute" and answers with a whole-set pass.
func AccountForRecord(value []byte) (accountID string, ok bool) {
	var record vm.InstanceRecord
	if err := json.Unmarshal(value, &record); err != nil {
		return "", false
	}
	if record.Metadata.AccountID == "" || record.Metadata.AccountID == utils.GlobalAccountID {
		return "", false
	}
	return record.Metadata.AccountID, true
}
