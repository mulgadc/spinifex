// Package handlers_rds holds the RDS control plane: the KV state layout, the
// ARN and lifecycle-status models, and the db.* instance-class facade over the
// platform's EC2 sizing table. It is engine-agnostic — PostgreSQL specifics live
// behind the agent/AMI seam, not here.
package handlers_rds

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// JetStream KV bucket constants for the RDS control plane (rds-v1.md D3).
//
// Per-account bucket "rds-account-{accountID}" holds all customer-visible DB
// instance, snapshot, subnet-group, parameter-group and automated-backup state.
// It is created lazily on first touch (no daemon-boot pre-creation) so accounts
// with no DB instances never grow a bucket.
//
// System bucket "rds-system" holds the instanceID → DB instance reverse index.
// The in-guest agent's IMDS credentials are minted under the system account, so
// its session name yields an instance ID with no account attached; without the
// index every heartbeat would have to enumerate every KV bucket in the cluster
// to find its own record. The index makes it a single Get.
const (
	KVBucketRDSAccountPrefix  = "rds-account-"
	KVBucketRDSAccountVersion = 1
	KVBucketRDSAccountHistory = 1

	KVBucketRDSSystem        = "rds-system"
	KVBucketRDSSystemVersion = 1
	KVBucketRDSSystemHistory = 1
)

// Key-path helpers for the per-account bucket. The layout matches rds-v1.md D3:
//
//	db-instances/{dbInstanceIdentifier}
//	db-snapshots/{dbSnapshotIdentifier}
//	db-subnet-groups/{name}
//	db-parameter-groups/{name}/meta
//	db-parameter-groups/{name}/params/{key}
//	backups/{dbInstanceIdentifier}/automated/{ts}
//	retained-volumes/{volumeID}
//
// Tags live inline on each resource's own record rather than in a separate key
// space, so there is no tags/ prefix here.

// DBInstancesPrefix returns the KV key prefix under which all DB instance
// records live. Used by DescribeDBInstances and the reconciler to enumerate.
func DBInstancesPrefix() string {
	return "db-instances/"
}

// DBInstanceKey returns the KV key for a DB instance record.
func DBInstanceKey(dbInstanceIdentifier string) string {
	return DBInstancesPrefix() + dbInstanceIdentifier
}

// DBSnapshotsPrefix returns the KV key prefix under which all DB snapshot
// records live. Manual and automated snapshots share this space, distinguished
// by the snapshot type on the record.
func DBSnapshotsPrefix() string {
	return "db-snapshots/"
}

// DBSnapshotKey returns the KV key for a DB snapshot record.
func DBSnapshotKey(dbSnapshotIdentifier string) string {
	return DBSnapshotsPrefix() + dbSnapshotIdentifier
}

// DBSubnetGroupsPrefix returns the KV key prefix under which all DB subnet
// group records live.
func DBSubnetGroupsPrefix() string {
	return "db-subnet-groups/"
}

// DBSubnetGroupKey returns the KV key for a DB subnet group record.
func DBSubnetGroupKey(name string) string {
	return DBSubnetGroupsPrefix() + name
}

// DBParameterGroupsPrefix returns the KV key prefix under which all DB
// parameter groups live. A group's own record is at .../meta and its values
// hang off .../params/, so listing groups walks the meta keys.
func DBParameterGroupsPrefix() string {
	return "db-parameter-groups/"
}

// DBParameterGroupMetaKey returns the KV key for a parameter group's metadata
// record (family, description, tags).
func DBParameterGroupMetaKey(name string) string {
	return fmt.Sprintf("%s%s/meta", DBParameterGroupsPrefix(), name)
}

// DBParameterGroupParamsPrefix returns the KV key prefix under which one
// parameter group's customer-set values live. Values are one key each rather
// than one blob so a ModifyDBParameterGroup touching a single parameter cannot
// clobber a concurrent change to another.
func DBParameterGroupParamsPrefix(name string) string {
	return fmt.Sprintf("%s%s/params/", DBParameterGroupsPrefix(), name)
}

// DBParameterGroupParamKey returns the KV key for one parameter's value within
// a parameter group.
func DBParameterGroupParamKey(name, param string) string {
	return DBParameterGroupParamsPrefix(name) + param
}

// AutomatedBackupsPrefix returns the KV key prefix under which one DB
// instance's automated-backup index entries live. Kept separate from
// db-snapshots/ so the retention sweep enumerates only automated backups.
func AutomatedBackupsPrefix(dbInstanceIdentifier string) string {
	return fmt.Sprintf("backups/%s/automated/", dbInstanceIdentifier)
}

// AutomatedBackupKey returns the KV key for one automated-backup index entry.
// ts is the backup timestamp, which orders the entries lexically for the
// retention sweep.
func AutomatedBackupKey(dbInstanceIdentifier, ts string) string {
	return AutomatedBackupsPrefix(dbInstanceIdentifier) + ts
}

// RetainedVolumesPrefix returns the KV key prefix under which data volumes held
// alive by surviving snapshots live (D10). A COW snapshot references its source
// volume's chunk files, so deleting a DB instance cannot delete a volume any
// snapshot still points at.
func RetainedVolumesPrefix() string {
	return "retained-volumes/"
}

// RetainedVolumeKey returns the KV key for a retained data volume record.
func RetainedVolumeKey(volumeID string) string {
	return RetainedVolumesPrefix() + volumeID
}

// InstanceIndexPrefix returns the KV key prefix of the reverse index in the
// system bucket. Entries are rewritten on every VM replace (each mints a new
// instance ID) and removed at teardown.
func InstanceIndexPrefix() string {
	return "instance-index/"
}

// InstanceIndexKey returns the system-bucket key mapping an internal EC2
// instance ID to the account and DB instance it belongs to.
func InstanceIndexKey(instanceID string) string {
	return InstanceIndexPrefix() + instanceID
}

// Store is the per-daemon RDS KV handle. Per-account and system buckets are
// accessed via the package-level factories below.
type Store struct {
	nc *nats.Conn
}

// NewStore constructs a Store bound to the supplied NATS connection. It does not
// touch JetStream — buckets are created lazily by the factories below.
func NewStore(nc *nats.Conn) (*Store, error) {
	if nc == nil {
		return nil, errors.New("rds store: nats connection is nil")
	}
	return &Store{nc: nc}, nil
}

// AccountBucketName returns the per-account JetStream KV bucket name for the
// given AWS account ID.
func AccountBucketName(accountID string) string {
	return KVBucketRDSAccountPrefix + accountID
}

// GetOrCreateAccountBucket returns the per-account KV bucket for accountID,
// creating it on first use. Idempotent: subsequent calls with the same accountID
// return the existing handle.
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

// GetOrCreateSystemBucket returns the shared rds-system bucket holding the
// instance-index reverse lookup. Created lazily alongside the first DB instance
// rather than at daemon boot, so a cluster with no RDS usage carries no bucket.
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
