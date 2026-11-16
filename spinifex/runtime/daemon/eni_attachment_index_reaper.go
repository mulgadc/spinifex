package daemon

import (
	"context"
	"time"

	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// eniAttachmentIndexReaper converges the per-instance attachment index that
// DescribeInstances reads against the ENI records: it indexes an attached
// interface the index lacks and prunes entries whose interface has moved on.
type eniAttachmentIndexReaper struct {
	vpc    *ec2vpc.VPCServiceImpl
	minAge time.Duration
}

var _ vm.Reaper = (*eniAttachmentIndexReaper)(nil)

// newENIAttachmentIndexReaper returns nil when this node has no VPC service.
func (d *Daemon) newENIAttachmentIndexReaper() *eniAttachmentIndexReaper {
	if d.vpcService == nil {
		return nil
	}
	return &eniAttachmentIndexReaper{vpc: d.vpcService, minAge: ec2vpc.AttachmentIndexMinAge}
}

func (r *eniAttachmentIndexReaper) Class() string         { return "eni-attachment-index" }
func (r *eniAttachmentIndexReaper) Scope() vm.ReaperScope { return vm.ScopeClusterWide }

// Sweep reports the entries it created and pruned together.
func (r *eniAttachmentIndexReaper) Sweep(ctx context.Context) (int, error) {
	indexed, pruned, err := r.vpc.ReconcileAttachmentIndex(ctx, r.minAge)
	return indexed + pruned, err
}
