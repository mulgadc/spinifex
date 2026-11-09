package daemon

import (
	"context"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/domains/dns"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	ecsdomain "github.com/mulgadc/spinifex/spinifex/domains/ecs"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	reconcilelock "github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	route53domain "github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/nats-io/nats.go/jetstream"
)

// daemonLocks is every lease and claim the daemon owns, each constructed once
// against the process's sync bucket and handed to the sites that run under it.
type daemonLocks struct {
	dns            *kvsync.Lease
	route53Realize *kvsync.Lease
	route53Verify  *kvsync.Lease
	rds            *kvsync.Lease
	ecs            *kvsync.Lease
	ochre          *kvsync.Lease
	elbv2          *kvsync.Lease
	eks            *kvsync.LeaseGroup
	uefi           *kvsync.ClaimGroup
}

func newDaemonLocks(store kvsync.KVStore, node string) *daemonLocks {
	return &daemonLocks{
		dns:            dns.NewReconcileLease(store, node),
		route53Realize: reconcilelock.NewLeaderLease(store, route53domain.KeyRealize, node),
		route53Verify:  reconcilelock.NewLeaderLease(store, route53domain.KeyVerify, node),
		rds:            rdsinstance.NewReconcilerLease(store, node),
		ecs:            ecsdomain.NewSchedulerLease(store, node),
		ochre:          ochre.NewReaperLease(store, node),
		elbv2:          elbv2domain.NewDataPathLease(store, node),
		eks:            ekscluster.NewClusterLeases(store, node),
		uefi:           uefistore.NewClaims(store, node),
	}
}

// obsoleteBuckets held only keys nothing reads any more: DHCP leases, and the
// lease buckets every election ran in before they moved to the sync bucket.
var obsoleteBuckets = []string{
	"spinifex-dhcp-leases",
	"spinifex-vpcd-reconcile",
	"spinifex-dns-reconcile",
	"spinifex-route53-realize",
	"spinifex-route53-verify",
	"spinifex-quota-reconcile",
	"spinifex-rds-leader",
	"spinifex-ecs-leader",
	"bedrock-leader",
	"spinifex-eks-leader",
	"spinifex-elbv2-leader",
}

// deleteObsoleteBuckets removes each obsolete bucket that exists. A failure is
// logged and the rest are still tried; the next start retries it.
func deleteObsoleteBuckets(ctx context.Context, js jetstream.JetStream) {
	for _, name := range obsoleteBuckets {
		if err := kvstore.DeleteBucketIfExists(ctx, js, name); err != nil {
			slog.WarnContext(ctx, "Failed to delete obsolete KV bucket", "bucket", name, "err", err)
		}
	}
}

// uefiClaims is the UEFI store claim group, or nil before startCluster has
// built the daemon's locks.
func (d *Daemon) uefiClaims() *kvsync.ClaimGroup {
	if d.locks == nil {
		return nil
	}
	return d.locks.uefi
}
