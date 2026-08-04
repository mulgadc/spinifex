package handlers_rds

import (
	"context"
	"log/slog"

	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/nats-io/nats.go/jetstream"
)

// The vanity hostname for a DB instance, or "" on a deployment with no base
// domain — where the endpoint is the bare ENI IP instead.
func (s *Service) dnsName(accountID, dbInstanceIdentifier string) string {
	if s.baseDomain == "" {
		return ""
	}
	return handlers_dns.RDSName(dbInstanceIdentifier, accountID, s.region, s.baseDomain)
}

// Best-effort: a miss is repaired by the reconcile backstop within a cycle, and
// a create must not fail because northstar was briefly unreachable.
func (s *Service) publishDNS(ctx context.Context, accountID string, rec *DBInstanceRecord, action handlers_dns.Action) {
	if s.baseDomain == "" || rec == nil || rec.DNSName == "" || rec.ENIPrivateIP == "" {
		return
	}
	changes := handlers_dns.RDSChanges(action, rec.DNSName, s.baseDomain, rec.ENIPrivateIP)
	slog.DebugContext(ctx, "rds: publishing endpoint record",
		"dbInstance", rec.DBInstanceIdentifier, "name", rec.DNSName, "action", action)
	handlers_dns.PublishChangesBestEffort(s.nc, accountID, changes)
}

// The UPSERTs for every endpoint-ready DB instance across all account buckets,
// plus whether the enumeration was authoritative. Instances live in per-account
// buckets, so a complete cross-tenant view requires reading every one: any
// failure yields ok=false, which suppresses RDS pruning rather than deleting a
// tenant's endpoint on a partial view.
func (s *Service) DesiredDNSChanges() (changes []handlers_dns.Change, ok bool) {
	if s == nil || s.baseDomain == "" {
		return nil, false
	}
	ctx := context.Background()
	js, err := s.js()
	if err != nil {
		return nil, false
	}
	buckets, err := AccountBucketNames(ctx, js)
	if err != nil {
		slog.Warn("rds DesiredDNSChanges: enumerate account buckets", "err", err)
		return nil, false
	}
	for _, bucket := range buckets {
		kv, err := js.KeyValue(ctx, bucket)
		if err != nil {
			slog.Warn("rds DesiredDNSChanges: open account bucket", "bucket", bucket, "err", err)
			return nil, false
		}
		bucketChanges, err := desiredBucketDNSChanges(ctx, kv, s.baseDomain)
		if err != nil {
			slog.Warn("rds DesiredDNSChanges: read DB instances", "bucket", bucket, "err", err)
			return nil, false
		}
		changes = append(changes, bucketChanges...)
	}
	return changes, true
}

// A deleted instance's record is gone, so it contributes nothing and the
// reconcile prunes its record. Anything still holding an ENI IP keeps its name
// resolvable, including a failed instance an operator is still investigating.
func desiredBucketDNSChanges(ctx context.Context, kv jetstream.KeyValue, baseDomain string) ([]handlers_dns.Change, error) {
	ids, err := ListDBInstanceIDs(ctx, kv)
	if err != nil {
		return nil, err
	}
	var changes []handlers_dns.Change
	for _, id := range ids {
		var rec DBInstanceRecord
		found, err := getJSON(ctx, kv, DBInstanceKey(id), &rec)
		if err != nil {
			return nil, err
		}
		if !found || rec.Status == StatusDeleted || rec.DNSName == "" || rec.ENIPrivateIP == "" {
			continue
		}
		changes = append(changes, handlers_dns.RDSChanges(
			handlers_dns.ActionUpsert, rec.DNSName, baseDomain, rec.ENIPrivateIP)...)
	}
	return changes, nil
}
