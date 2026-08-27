// Package handlers_rds holds the RDS control plane: the KV state layout, the
// ARN and lifecycle-status models, and the db.* instance-class facade over the
// platform's EC2 sizing table. It is engine-agnostic — PostgreSQL specifics live
// behind the agent/AMI seam, not here.
package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// "rds-account-{accountID}" holds customer-visible state. "rds-system" holds
// the instanceID → DB instance reverse index: agent IMDS credentials are minted
// under the system account, so without it a heartbeat would scan every bucket.
const (
	KVBucketRDSAccountPrefix  = "rds-account-"
	KVBucketRDSAccountVersion = 1
	KVBucketRDSAccountHistory = 1

	KVBucketRDSSystem        = "rds-system"
	KVBucketRDSSystemVersion = 1
	KVBucketRDSSystemHistory = 1

	// "spinifex-rds-leader" holds the single reconciler lease. Its TTL expires a
	// lease whose holder died mid-cycle, so control work resumes without an
	// operator.
	KVBucketRDSLeader        = "spinifex-rds-leader"
	KVBucketRDSLeaderVersion = 1
	KVBucketRDSLeaderTTL     = 60 * time.Second
)

// Key-path helpers for the per-account bucket:
//
//	db-instances/{dbInstanceIdentifier}
//	bootstrap-payloads/{dbInstanceIdentifier}
//	db-snapshots/{dbSnapshotIdentifier}
//	db-subnet-groups/{name}
//	db-parameter-groups/{name}/meta
//	db-parameter-groups/{name}/params/{key}
//	backups/{dbInstanceIdentifier}/automated/{ts}
//	retained-volumes/{volumeID}
//	events/{sourceType}/{sourceIdentifier}
//
// Tags live inline on each resource's own record rather than in a separate key
// space, so there is no tags/ prefix here.

func DBInstancesPrefix() string {
	return "db-instances/"
}

func DBInstanceKey(dbInstanceIdentifier string) string {
	return DBInstancesPrefix() + dbInstanceIdentifier
}

// The encrypted bootstrap payload, kept out of the instance record so it has its
// own CAS domain and so a daemon that predates it cannot drop it through a
// read-modify-marshal of the record.
func BootstrapPayloadsPrefix() string {
	return "bootstrap-payloads/"
}

func BootstrapPayloadKey(dbInstanceIdentifier string) string {
	return BootstrapPayloadsPrefix() + dbInstanceIdentifier
}

// Manual and automated snapshots share this space, distinguished by the
// snapshot type on the record.
func DBSnapshotsPrefix() string {
	return "db-snapshots/"
}

func DBSnapshotKey(dbSnapshotIdentifier string) string {
	return DBSnapshotsPrefix() + kvKeySegment(dbSnapshotIdentifier)
}

// A JetStream KV key admits no colon, and an automated backup's snapshot
// identifier carries the rds: prefix AWS gives it. The colon is mapped to ".",
// the separator other stores compose keys from, and one no RDS identifier may
// contain — so the mapping is reversible and a manual snapshot's key is unchanged.
const kvKeyColon = "."

func kvKeySegment(identifier string) string {
	return strings.ReplaceAll(identifier, ":", kvKeyColon)
}

func kvKeySegmentIdentifier(segment string) string {
	return strings.ReplaceAll(segment, kvKeyColon, ":")
}

func DBSubnetGroupsPrefix() string {
	return "db-subnet-groups/"
}

func DBSubnetGroupKey(name string) string {
	return DBSubnetGroupsPrefix() + name
}

// A group's own record is at .../meta and its values hang off .../params/, so
// listing groups walks the meta keys.
func DBParameterGroupsPrefix() string {
	return "db-parameter-groups/"
}

func DBParameterGroupMetaKey(name string) string {
	return fmt.Sprintf("%s%s/meta", DBParameterGroupsPrefix(), name)
}

// One key per value rather than one blob, so a ModifyDBParameterGroup touching
// a single parameter cannot clobber a concurrent change to another.
func DBParameterGroupParamsPrefix(name string) string {
	return fmt.Sprintf("%s%s/params/", DBParameterGroupsPrefix(), name)
}

func DBParameterGroupParamKey(name, param string) string {
	return DBParameterGroupParamsPrefix(name) + param
}

// Kept separate from db-snapshots/ so the retention sweep enumerates only
// automated backups, ordered lexically by their timestamp suffix.
func AutomatedBackupsRootPrefix() string {
	return "backups/"
}

func AutomatedBackupsPrefix(dbInstanceIdentifier string) string {
	return fmt.Sprintf("%s%s/automated/", AutomatedBackupsRootPrefix(), dbInstanceIdentifier)
}

func AutomatedBackupKey(dbInstanceIdentifier, ts string) string {
	return AutomatedBackupsPrefix(dbInstanceIdentifier) + ts
}

// Data volumes held alive by surviving snapshots: a COW snapshot references its
// source volume's chunks, so deleting a DB instance cannot delete the volume.
func RetainedVolumesPrefix() string {
	return "retained-volumes/"
}

func RetainedVolumeKey(volumeID string) string {
	return RetainedVolumesPrefix() + volumeID
}

// One bounded ring per resource rather than a key per event: DescribeEvents
// always reads a whole resource's history, and a key per event would make the
// 14-day trim a listing plus a delete per expired entry.
//
// The ring is deliberately outside the resource's own record, so a deleted DB
// instance's events survive it for the rest of the retention window.
func EventsPrefix() string {
	return "events/"
}

// The source identifier is escaped the same way a snapshot key is: an automated
// backup's events hang off an identifier carrying a colon.
func EventRingKey(sourceType, sourceIdentifier string) string {
	return fmt.Sprintf("%s%s/%s", EventsPrefix(), sourceType, kvKeySegment(sourceIdentifier))
}

// Entries are rewritten on every VM replace (each mints a new instance ID) and
// removed at teardown.
func InstanceIndexPrefix() string {
	return "instance-index/"
}

func InstanceIndexKey(instanceID string) string {
	return InstanceIndexPrefix() + instanceID
}

type Store struct {
	nc *nats.Conn
}

// Does not touch JetStream — buckets are created lazily by the factories below.
func NewStore(nc *nats.Conn) (*Store, error) {
	if nc == nil {
		return nil, errors.New("rds store: nats connection is nil")
	}
	return &Store{nc: nc}, nil
}

func AccountBucketName(accountID string) string {
	return KVBucketRDSAccountPrefix + accountID
}

// Creates the bucket on first use; subsequent calls return the existing handle.
func GetOrCreateAccountBucket(ctx context.Context, js jetstream.JetStream, accountID string) (jetstream.KeyValue, error) {
	bucket := AccountBucketName(accountID)
	kv, err := kvutil.GetOrCreateBucket(ctx, js, bucket, KVBucketRDSAccountHistory)
	if err != nil {
		return nil, fmt.Errorf("failed to create RDS per-account KV bucket %s: %w", bucket, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, bucket, kv, KVBucketRDSAccountVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", bucket, err)
	}
	return kv, nil
}

// Created lazily alongside the first DB instance rather than at daemon boot, so
// a cluster with no RDS usage carries no bucket.
func GetOrCreateSystemBucket(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketRDSSystem, KVBucketRDSSystemHistory)
	if err != nil {
		return nil, fmt.Errorf("failed to create RDS system KV bucket %s: %w", KVBucketRDSSystem, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketRDSSystem, kv, KVBucketRDSSystemVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketRDSSystem, err)
	}
	return kv, nil
}

// kvutil.GetOrCreateBucket exposes no TTL knob, so the lease bucket attaches
// directly and creates only when absent.
func InitLeaderBucket(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	// Attach before create. This runs on every reconcile tick, and CreateKeyValue
	// against a bucket that exists with any other config is a STREAM.CREATE the
	// meta leader answers with an error, so creating first bills one per tick.
	kv, err := js.KeyValue(ctx, KVBucketRDSLeader)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  KVBucketRDSLeader,
			History: 1,
			TTL:     KVBucketRDSLeaderTTL,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create or open RDS leader bucket %s: %w", KVBucketRDSLeader, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketRDSLeader, kv, KVBucketRDSLeaderVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketRDSLeader, err)
	}
	return kv, nil
}

// Every RDS per-account bucket in the cluster. The reconciler and the DNS
// desired-set both need a cross-tenant view, which only this provides.
func AccountBucketNames(ctx context.Context, js jetstream.JetStream) ([]string, error) {
	all, err := kvutil.BucketNames(ctx, js)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(all))
	for _, name := range all {
		if strings.HasPrefix(name, KVBucketRDSAccountPrefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// AccountWatchBuckets returns every per-account bucket as a watchable handle,
// for a reconciler that must be woken by a cluster change rather than poll for
// it. The set is re-read on each call because a new account's bucket appears
// without notice: JetStream publishes no bucket-created event.
func AccountWatchBuckets(ctx context.Context, js jetstream.JetStream) ([]*kvstore.Bucket, error) {
	names, err := AccountBucketNames(ctx, js)
	if err != nil {
		return nil, err
	}
	buckets := make([]*kvstore.Bucket, 0, len(names))
	for _, name := range names {
		buckets = append(buckets, kvstore.NewBucket(js, kvstore.Config{Name: name, History: 1}))
	}
	return buckets, nil
}

// The account a per-account bucket belongs to, for callers that enumerated
// buckets rather than starting from an account.
func AccountIDFromBucketName(bucket string) string {
	return strings.TrimPrefix(bucket, KVBucketRDSAccountPrefix)
}

// The DB instance identifiers held in one account bucket. An empty bucket
// yields no names rather than an error.
func ListDBInstanceIDs(ctx context.Context, kv jetstream.KeyValue) ([]string, error) {
	return listNames(ctx, kv, DBInstancesPrefix())
}

// Manual and automated snapshots alike: the type is on the record, so a listing
// filtered by it still has to read every one.
func ListDBSnapshotIDs(ctx context.Context, kv jetstream.KeyValue) ([]string, error) {
	names, err := listNames(ctx, kv, DBSnapshotsPrefix())
	if err != nil {
		return nil, err
	}
	for i, name := range names {
		names[i] = kvKeySegmentIdentifier(name)
	}
	return names, nil
}

// Walks the .../meta keys, which is what makes a group's own record findable
// among the per-parameter keys hanging off the same prefix.
func ListDBParameterGroupNames(ctx context.Context, kv jetstream.KeyValue) ([]string, error) {
	keys, err := bucketKeys(ctx, kv)
	if err != nil {
		return nil, err
	}
	prefix := DBParameterGroupsPrefix()
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "/meta") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "/meta")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// The stored overrides of one parameter group, keyed by parameter name.
func ListDBParameterOverrides(ctx context.Context, kv jetstream.KeyValue, group string) (map[string]DBParameterRecord, error) {
	prefix := DBParameterGroupParamsPrefix(group)
	names, err := listNames(ctx, kv, prefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string]DBParameterRecord, len(names))
	for _, name := range names {
		var rec DBParameterRecord
		found, err := getJSON(ctx, kv, prefix+name, &rec)
		if err != nil {
			return nil, err
		}
		// A parameter reset between the listing and this read is simply gone,
		// which is the answer a resolve one tick later would give too.
		if found {
			out[name] = rec
		}
	}
	return out, nil
}

// Every account's automated-backup index, grouped by DB instance, from one
// bucket listing: the retention sweep needs every instance's set, and a Keys call
// per instance would cost one listing each.
func ListAutomatedBackups(ctx context.Context, kv jetstream.KeyValue) (map[string][]string, error) {
	keys, err := bucketKeys(ctx, kv)
	if err != nil {
		return nil, err
	}
	indexed := make(map[string][]string)
	for _, key := range keys {
		id, stamp, ok := splitAutomatedBackupKey(key)
		if !ok {
			continue
		}
		indexed[id] = append(indexed[id], stamp)
	}
	return indexed, nil
}

// The DB instance and timestamp a backups/ key names. The instance identifier
// cannot contain a slash, so anything shaped otherwise belongs to a key space
// this does not own.
func splitAutomatedBackupKey(key string) (string, string, bool) {
	rest, ok := strings.CutPrefix(key, AutomatedBackupsRootPrefix())
	if !ok {
		return "", "", false
	}
	id, rest, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		return "", "", false
	}
	stamp, ok := strings.CutPrefix(rest, "automated/")
	if !ok || stamp == "" || strings.Contains(stamp, "/") {
		return "", "", false
	}
	return id, stamp, true
}

// The leaf names directly under prefix. A nested key belongs to a sub-space and
// is skipped rather than reported as a malformed name.
func listNames(ctx context.Context, kv jetstream.KeyValue, prefix string) ([]string, error) {
	keys, err := bucketKeys(ctx, kv)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		name := strings.TrimPrefix(key, prefix)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// An empty bucket yields no keys rather than an error, so a first-ever describe
// answers with an empty list instead of failing.
func bucketKeys(ctx context.Context, kv jetstream.KeyValue) ([]string, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("rds: list keys: %w", err)
	}
	return keys, nil
}
