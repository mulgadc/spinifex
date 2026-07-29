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
)

// Key-path helpers for the per-account bucket:
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

func DBInstancesPrefix() string {
	return "db-instances/"
}

func DBInstanceKey(dbInstanceIdentifier string) string {
	return DBInstancesPrefix() + dbInstanceIdentifier
}

// Manual and automated snapshots share this space, distinguished by the
// snapshot type on the record.
func DBSnapshotsPrefix() string {
	return "db-snapshots/"
}

func DBSnapshotKey(dbSnapshotIdentifier string) string {
	return DBSnapshotsPrefix() + dbSnapshotIdentifier
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
func AutomatedBackupsPrefix(dbInstanceIdentifier string) string {
	return fmt.Sprintf("backups/%s/automated/", dbInstanceIdentifier)
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
