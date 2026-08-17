package accountteardown

import (
	"context"
	"fmt"
	"log/slog"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NewClusterEngine wires every reaper against a live cluster.
//
// The CLI and the admin API both build their engine here rather than each
// assembling its own list: a reaper registered on one surface and not the other
// would leave resources behind depending on how the teardown was started.
func NewClusterEngine(
	ctx context.Context,
	nc *nats.Conn,
	expectedNodes int,
	svc handlers_iam.IAMService,
	buckets BucketStore,
) (*Engine, error) {
	// Refused rather than skipped. A teardown missing its bucket reaper still
	// reports every stage drained and still deletes the account record, leaving
	// the tenant's objects behind with nothing able to attribute them to anyone.
	if buckets == nil {
		return nil, fmt.Errorf("teardown needs an object store to reap buckets with")
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("get JetStream context: %w", err)
	}

	names, err := handlers_iam.NewAccountNameIndex(ctx, js)
	if err != nil {
		return nil, fmt.Errorf("open account name index: %w", err)
	}

	// Absent when the cluster never enabled quotas, which is not a reason to
	// refuse a teardown: there is simply no counter to remove.
	usage, err := kvutil.GetOrCreateBucket(ctx, js, handlers_quota.KVBucketAccountUsage, 1)
	if err != nil {
		slog.WarnContext(ctx, "Teardown: quota usage bucket unavailable, its counter will not be removed", "err", err)
		usage = nil
	}

	// Managed services first: their teardown stops the tasks and releases the
	// ENIs riding on instances the EC2 reaper is about to terminate, and their
	// compute does not always belong to the tenant, so the instance reaper
	// cannot see it.
	reapers := BedrockReapers(nc)
	reapers = append(reapers, ECSReapers(nc)...)
	reapers = append(reapers, EKSReapers(nc)...)
	reapers = append(reapers, RDSReapers(nc)...)

	// Before EC2, and the order matters within each stage because the sort that
	// groups reapers by stage is stable. Load balancers and NAT gateways hold
	// the subnets, security groups and addresses EC2's reapers delete, and a
	// live spot request launches a replacement for anything already terminated.
	reapers = append(reapers, ELBv2Reapers(nc)...)
	reapers = append(reapers, EC2ExtraReapers(nc)...)
	reapers = append(reapers, EC2Reapers(nc, expectedNodes)...)

	reapers = append(reapers, S3Reapers(buckets)...)
	reapers = append(reapers, ACMReapers(nc)...)
	reapers = append(reapers, IAMReapers(svc)...)
	reapers = append(reapers, AccountReapers(svc, names, usage)...)

	return NewEngine(NewIAMAccounts(svc), reapers...), nil
}
