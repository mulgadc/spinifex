package handlers_ecs

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// JetStream KV bucket and key-path constants for the ECS control plane.
//
// Per-account bucket "ecs-account-{accountID}" holds all customer-visible
// cluster, task-definition, task, service, and container-instance state. It is
// created lazily on first cluster touch (no daemon-boot pre-creation) so accounts
// without any ECS clusters never grow a bucket (ecs-v1.md Q2).
//
// Shared leader bucket "spinifex-ecs-leader" holds 60s-TTL CAS locks keyed by
// "{accountID}/{clusterName}" for the per-cluster scheduler leader election
// (Q3). Created once at daemon boot.
const (
	KVBucketECSAccountPrefix  = "ecs-account-"
	KVBucketECSAccountVersion = 1
	KVBucketECSAccountHistory = 1

	KVBucketECSLeader        = "spinifex-ecs-leader"
	KVBucketECSLeaderVersion = 1
	KVBucketECSLeaderTTL     = 60 * time.Second
)

// Key-path helpers for the per-account bucket. The layout matches ecs-v1.md Q2.
// Task definitions are account-scoped (not cluster-scoped) per the AWS ARN shape:
//
//	clusters/{name}/meta
//	clusters/{name}/instances/{instanceID}
//	clusters/{name}/tasks/{taskID}
//	clusters/{name}/services/{serviceName}
//	taskdef-families/{family}/latest-rev
//	taskdef-families/{family}/revs/{rev}

// ClusterMetaKey returns the KV key for a cluster's meta record.
func ClusterMetaKey(name string) string {
	return fmt.Sprintf("clusters/%s/meta", name)
}

// InstancesPrefix returns the KV key prefix under which all of a cluster's
// container-instance records live. Used by ListContainerInstances to enumerate.
func InstancesPrefix(cluster string) string {
	return fmt.Sprintf("clusters/%s/instances/", cluster)
}

// InstanceKey returns the KV key for a container-instance record under a cluster.
func InstanceKey(cluster, instanceID string) string {
	return InstancesPrefix(cluster) + instanceID
}

// TasksPrefix returns the KV key prefix under which all of a cluster's task
// records live. Used by ListTasks and the agent reboot reconciler to enumerate.
func TasksPrefix(cluster string) string {
	return fmt.Sprintf("clusters/%s/tasks/", cluster)
}

// TaskKey returns the KV key for a task record under a cluster.
func TaskKey(cluster, taskID string) string {
	return TasksPrefix(cluster) + taskID
}

// AssignmentsPrefix returns the KV key prefix under which a container instance's
// pending task assignments (its "inbox") live. Kept in a sibling path to
// instances/ + tasks/ so listing those records never picks up assignment keys.
// The agent drains this inbox by polling the gateway (no direct NATS).
func AssignmentsPrefix(cluster, instanceID string) string {
	return fmt.Sprintf("clusters/%s/assignments/%s/", cluster, instanceID)
}

// AssignmentKey returns the KV key for one task's assignment in an instance inbox.
func AssignmentKey(cluster, instanceID, taskID string) string {
	return AssignmentsPrefix(cluster, instanceID) + taskID
}

// StopsPrefix returns the KV key prefix under which a container instance's pending
// task-stop directives (its "stop inbox") live. A sibling of assignments/ so it is
// never picked up when listing tasks or assignments. The agent drains it by
// polling the gateway; the STOPPED transition removes an entry.
func StopsPrefix(cluster, instanceID string) string {
	return fmt.Sprintf("clusters/%s/stops/%s/", cluster, instanceID)
}

// StopKey returns the KV key for one task's stop directive in an instance inbox.
func StopKey(cluster, instanceID, taskID string) string {
	return StopsPrefix(cluster, instanceID) + taskID
}

// ServicesPrefix returns the KV key prefix under which all of a cluster's service
// records live. Used by ListServices to enumerate.
func ServicesPrefix(cluster string) string {
	return fmt.Sprintf("clusters/%s/services/", cluster)
}

// ServiceKey returns the KV key for a service record under a cluster.
func ServiceKey(cluster, serviceName string) string {
	return ServicesPrefix(cluster) + serviceName
}

// TaskDefFamiliesPrefix returns the KV key prefix under which all task-definition
// families live. Task definitions are account-scoped, not cluster-scoped (Q2).
func TaskDefFamiliesPrefix() string {
	return "taskdef-families/"
}

// TaskDefLatestRevKey returns the KV key holding a family's latest revision
// number. Read-modify-written on RegisterTaskDefinition.
func TaskDefLatestRevKey(family string) string {
	return fmt.Sprintf("taskdef-families/%s/latest-rev", family)
}

// TaskDefRevsPrefix returns the KV key prefix under which all revisions of a
// task-definition family live. Used by ListTaskDefinitions to enumerate.
func TaskDefRevsPrefix(family string) string {
	return fmt.Sprintf("taskdef-families/%s/revs/", family)
}

// TaskDefRevKey returns the KV key for a specific revision of a task-definition
// family.
func TaskDefRevKey(family string, rev int) string {
	return fmt.Sprintf("%s%d", TaskDefRevsPrefix(family), rev)
}

// LeaderLeaseKey returns the per-cluster scheduler leader-lease key in the shared
// leader bucket (Q3).
func LeaderLeaseKey(accountID, clusterName string) string {
	return fmt.Sprintf("%s/%s", accountID, clusterName)
}

// CapacityProvidersPrefix returns the KV key prefix under which all
// capacity-provider records live. Account-scoped (not cluster-scoped),
// matching the AWS ARN shape; a cluster references providers by name.
func CapacityProvidersPrefix() string {
	return "capacity-providers/"
}

// CapacityProviderKey returns the KV key for a named capacity-provider record.
func CapacityProviderKey(name string) string {
	return CapacityProvidersPrefix() + name
}

// Store is the per-daemon ECS KV handle. Per-account and leader buckets are
// accessed via the package-level factories below.
type Store struct {
	nc *nats.Conn
}

// NewStore constructs a Store bound to the supplied NATS connection. It does not
// touch JetStream — per-account buckets are created lazily by
// GetOrCreateAccountBucket and the leader bucket by InitLeaderBucket.
func NewStore(nc *nats.Conn) (*Store, error) {
	if nc == nil {
		return nil, errors.New("ecs store: nats connection is nil")
	}
	return &Store{nc: nc}, nil
}

// AccountBucketName returns the per-account JetStream KV bucket name for the
// given AWS account ID.
func AccountBucketName(accountID string) string {
	return KVBucketECSAccountPrefix + accountID
}

// GetOrCreateAccountBucket returns the per-account KV bucket for accountID,
// creating it on first use. Idempotent: subsequent calls with the same accountID
// return the existing handle.
func GetOrCreateAccountBucket(js nats.JetStreamContext, accountID string) (nats.KeyValue, error) {
	bucket := AccountBucketName(accountID)
	kv, err := utils.GetOrCreateKVBucket(js, bucket, KVBucketECSAccountHistory)
	if err != nil {
		return nil, fmt.Errorf("failed to create ECS per-account KV bucket %s: %w", bucket, err)
	}
	if err := migrate.DefaultRegistry.RunKV(bucket, kv, KVBucketECSAccountVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", bucket, err)
	}
	return kv, nil
}

// InitLeaderBucket creates (or attaches to) the shared spinifex-ecs-leader bucket
// used for per-cluster scheduler leader-lease CAS locks. The bucket is configured
// with History=1 and a 60s TTL so stale leases expire on their own when a leader
// dies mid-cycle. utils.GetOrCreateKVBucket doesn't expose a TTL knob, so this
// takes the direct js.CreateKeyValue path and falls back to js.KeyValue on
// already-exists.
func InitLeaderBucket(js nats.JetStreamContext) (nats.KeyValue, error) {
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:  KVBucketECSLeader,
		History: 1,
		TTL:     KVBucketECSLeaderTTL,
	})
	if err != nil {
		kv, err = js.KeyValue(KVBucketECSLeader)
		if err != nil {
			return nil, fmt.Errorf("failed to create or open ECS leader bucket %s: %w", KVBucketECSLeader, err)
		}
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketECSLeader, kv, KVBucketECSLeaderVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketECSLeader, err)
	}
	slog.Info("ECS leader bucket initialized", "bucket", KVBucketECSLeader, "ttl", KVBucketECSLeaderTTL)
	return kv, nil
}
