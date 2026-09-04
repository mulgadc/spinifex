package gateway_ec2_instance

import (
	"context"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/instancecache"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

const (
	// insufficientData is AWS's answer for a status the control plane cannot
	// determine, as distinct from impaired, which asserts something is wrong.
	insufficientData = "insufficient-data"
	statusImpaired   = "impaired"
)

// recordLister supplies the cached instance records synthesis reads. The
// bool reports cache readiness; a cold cache must not be treated as empty.
type recordLister interface {
	List(ctx context.Context, accountID string) ([]*vm.VM, bool)
}

// nodeLiveness reports whether the node that last ran an instance is still
// heartbeating.
type nodeLiveness interface {
	State(ctx context.Context, node string) instancecache.NodeState
}

// StatusSynthesis backfills status frames for instances no node answered for.
// The zero value disables it, so a gateway wired without a warm cache answers
// exactly as it did before.
type StatusSynthesis struct {
	Records  recordLister
	Liveness nodeLiveness
}

// fanoutResponders carries the DescribeInstanceStatus fan-out's own responder
// identity, so synthesis can tell a node that answered and omitted an
// instance from a node that never answered at all.
type fanoutResponders struct {
	// SuccessResponders lists nodes that answered with a decodable
	// non-error frame, i.e. actually ran their own selection logic.
	SuccessResponders map[string]bool
	// Unidentified is the count of frames received with no node ID header.
	// Above zero, responder identity cannot be trusted for this fan-out.
	Unidentified int
}

// synthesize returns status frames for cached instances that no live frame
// covered and whose owning node is not answering. It is a backfill, never a
// second opinion: an instance a node reported on is not touched.
func (s StatusSynthesis) synthesize(ctx context.Context, input *ec2.DescribeInstanceStatusInput, accountID string, covered map[string]bool, responders fanoutResponders) []*ec2.InstanceStatus {
	if s.Records == nil {
		return nil
	}

	// A cache that has not completed its first whole-set sync cannot support a
	// claim about what is missing, so it stays out of the answer entirely.
	instances, ready := s.Records.List(ctx, accountID)
	if !ready {
		return nil
	}

	// The fan-out already reported a malformed request through its own error
	// path. Adding nothing here avoids answering a request the node rejected.
	selection, err := handlers_ec2_instance.ParseStatusSelection(input, accountID)
	if err != nil {
		return nil
	}

	var out []*ec2.InstanceStatus
	for _, v := range instances {
		if v == nil || covered[v.ID] {
			continue
		}

		state := instancecache.NodeUnknown
		if s.Liveness != nil {
			state = s.Liveness.State(ctx, v.LastNode)
		}

		if responders.Unidentified > 0 {
			// Frames could not be attributed to nodes this fan-out, so the
			// responder set is not trustworthy: fall back to heartbeat age.
			if state == instancecache.NodeLive {
				continue
			}
		} else if responders.SuccessResponders[v.LastNode] {
			// The owning node answered this fan-out and chose not to report
			// this instance — that exclusion is deliberate, not silence.
			continue
		}

		// SystemImpaired is node-local memory pressure only an answering node
		// can report. A silent node's is unknowable, so it is not fabricated.
		status := selection.StatusFor(v, handlers_ec2_instance.StatusOptions{AZ: v.AZ})
		if status == nil {
			continue
		}
		degradeSystemStatus(status, state)
		out = append(out, status)
	}

	if len(out) > 0 {
		slog.InfoContext(ctx, "DescribeInstanceStatus: synthesised frames for unanswered instances",
			"count", len(out))
	}
	return out
}

// degradeSystemStatus marks a frame the owning node did not answer for. A
// stale node's instance is impaired; an unreadable heartbeat store leaves it
// insufficient-data, because not knowing a host is healthy is not knowing it
// is not.
func degradeSystemStatus(status *ec2.InstanceStatus, state instancecache.NodeState) {
	// A stopped instance stays not-applicable: node liveness says nothing
	// about an instance that is not meant to be running.
	if status.SystemStatus == nil || aws.StringValue(status.SystemStatus.Status) == notApplicable {
		return
	}

	label := insufficientData
	if state == instancecache.NodeStale {
		label = statusImpaired
	}
	status.SystemStatus.Status = aws.String(label)
	status.SystemStatus.Details = []*ec2.InstanceStatusDetails{{
		Name:   aws.String(reachabilityDetailName),
		Status: aws.String(label),
	}}
}

// coveredInstanceIDs indexes the instance IDs a node actually answered for, so
// synthesis can tell an unanswered instance from a deliberately excluded one.
func coveredInstanceIDs(statuses []*ec2.InstanceStatus) map[string]bool {
	covered := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		if s != nil && s.InstanceId != nil {
			covered[*s.InstanceId] = true
		}
	}
	return covered
}
