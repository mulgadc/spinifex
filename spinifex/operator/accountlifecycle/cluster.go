package accountlifecycle

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
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
	nodeIDs []string,
	svc iamdomain.IAMService,
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

	names, err := iamdomain.NewAccountNameIndex(ctx, js)
	if err != nil {
		return nil, fmt.Errorf("open account name index: %w", err)
	}

	// Absent when the cluster never enabled quotas, which is not a reason to
	// refuse a teardown: there is simply no counter to remove.
	usage, err := kvutil.GetOrCreateBucket(ctx, js, quota.KVBucketAccountUsage, 1)
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
	reapers = append(reapers, EC2Reapers(nc, expectedNodes, nodeIDs)...)

	reapers = append(reapers, S3Reapers(buckets)...)
	reapers = append(reapers, ACMReapers(nc)...)
	reapers = append(reapers, IAMReapers(svc)...)

	// Before the account counters and reservations: the seal must still be
	// able to find the account IAM holds as TERMINATING, which the account
	// record reaper does not remove.
	reapers = append(reapers, EC2AccountSettingsReapers(nc, nodeIDs)...)
	reapers = append(reapers, AccountReapers(svc, names, usage)...)

	engine := NewEngine(NewIAMAccounts(svc), reapers...)
	engine.ec2Inventory = newEC2PlatformInventoryClient(nc)
	return engine, nil
}
