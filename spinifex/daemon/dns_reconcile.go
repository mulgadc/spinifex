package daemon

import (
	"context"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// dnsWatchSources names the buckets whose changes should wake the DNS
// reconcile: one per input to dnsDesiredSet. Each is resolved lazily, because
// the reconciler is constructed before the services it reads from exist and
// re-enumerates its buckets on every resync anyway.
func (d *Daemon) dnsWatchSources() []reconciler.Source {
	return []reconciler.Source{
		reconciler.Dynamic(d.instanceStateWatchBuckets, instanceStateWatchFilter),
		reconciler.Dynamic(d.elbv2WatchBuckets, handlers_elbv2.KeyPrefixLB+"*"),
		reconciler.Dynamic(d.eksWatchBuckets, ">"),
		reconciler.Dynamic(d.rdsWatchBuckets, ">"),
	}
}

// instanceStateWatchFilter matches the instance records. EC2 records are the
// one DNS input that is node-local, so a change to any instance is the only
// signal that this node's own VMs may need re-asserting.
//
// This is a NATS subject filter, so "*" spans one dot-delimited token and the
// prefix has to end in a dot for it to match anything at all.
const instanceStateWatchFilter = InstanceRecordPrefix + "*"

// instanceStateWatchBuckets returns the shared instance-state bucket. It is a
// fresh kvstore.Bucket rather than the JetStreamManager's handle so a recovery
// swapping that handle cannot leave the watch pointing at a closed one.
func (d *Daemon) instanceStateWatchBuckets(context.Context) ([]*kvstore.Bucket, error) {
	if d.jsManager == nil || d.jsManager.js == nil {
		return nil, nil
	}
	return []*kvstore.Bucket{kvstore.NewBucket(d.jsManager.js, kvstore.Config{
		Name:     InstanceStateBucket,
		History:  1,
		Replicas: d.jsManager.replicas,
	})}, nil
}

func (d *Daemon) elbv2WatchBuckets(context.Context) ([]*kvstore.Bucket, error) {
	if d.elbv2Service == nil {
		return nil, nil
	}
	bucket := d.elbv2Service.DNSWatchBucket()
	if bucket == nil {
		return nil, nil
	}
	return []*kvstore.Bucket{bucket}, nil
}

func (d *Daemon) eksWatchBuckets(ctx context.Context) ([]*kvstore.Bucket, error) {
	if d.eksService == nil || d.natsConn == nil {
		return nil, nil
	}
	return handlers_eks.AccountWatchBuckets(ctx, d.natsConn)
}

func (d *Daemon) rdsWatchBuckets(ctx context.Context) ([]*kvstore.Bucket, error) {
	if d.rdsService == nil || d.jsManager == nil || d.jsManager.js == nil {
		return nil, nil
	}
	return handlers_rds.AccountWatchBuckets(ctx, d.jsManager.js)
}

// dnsDesiredSet builds the full desired managed-record set for the reconcile
// backstop. It spans all tenants: every running instance on this node plus
// every active load balancer, EKS cluster and DB instance across all accounts.
// Prune authority is granted per record class only when that class enumerated
// completely, so a transient store error can never delete another tenant's live
// records — the reconcile only ever repairs, never over-prunes, on a partial view.
func (d *Daemon) dnsDesiredSet() handlers_dns.DesiredSet {
	ds := handlers_dns.DesiredSet{}
	if ch, ok := d.desiredEC2DNSChanges(); ok {
		ds.Changes = append(ds.Changes, ch...)
		ds.Prunable.EC2 = true
	}

	if d.elbv2Service != nil {
		if ch, ok := d.elbv2Service.DesiredDNSChanges(); ok {
			ds.Changes = append(ds.Changes, ch...)
			ds.Prunable.ELB = true
		}
	}
	if d.eksService != nil {
		if ch, ok := d.eksService.DesiredDNSChanges(); ok {
			ds.Changes = append(ds.Changes, ch...)
			ds.Prunable.EKS = true
		}
	}
	if d.rdsService != nil {
		if ch, ok := d.rdsService.DesiredDNSChanges(); ok {
			ds.Changes = append(ds.Changes, ch...)
			ds.Prunable.RDS = true
		}
	}
	return ds
}

// desiredEC2DNSChanges returns UPSERTs for every running instance in the
// cluster, and whether that view was complete. The instance record key space
// spans all nodes, unlike the vmMgr map it replaces here, which is what lets
// the reconcile prune a record as well as repair one. The domains mirror the
// lifecycle publish so re-asserting is a no-op when in sync.
func (d *Daemon) desiredEC2DNSChanges() ([]handlers_dns.Change, bool) {
	if d.jsManager == nil {
		return nil, false
	}
	records, err := d.jsManager.ListInstanceRecords()
	if err != nil {
		// Reported without changes: a partial instance view is exactly the case
		// pruning must not run on, and repairing from it would be repairing from
		// the same partial view.
		slog.Warn("dns reconcile: instance records unavailable, EC2 records left alone this cycle",
			"error", err)
		return nil, false
	}

	var changes []handlers_dns.Change
	for _, record := range records {
		if record == nil || record.Status.Status != vm.StateRunning {
			continue
		}
		privateIP := ""
		if record.Status.Instance != nil {
			privateIP = aws.StringValue(record.Status.Instance.PrivateIpAddress)
		}
		if record.Status.PublicIP == "" && privateIP == "" {
			continue
		}
		changes = append(changes, handlers_dns.EC2Changes(
			handlers_dns.ActionUpsert, d.config.Region,
			d.dnsBaseDomain, d.dnsInternalDomain, record.Status.PublicIP, privateIP,
		)...)
	}
	return changes, true
}
