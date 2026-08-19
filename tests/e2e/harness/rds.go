//go:build e2e

package harness

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/daemon"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// The statuses a DB instance reports on its way up, the settled state a stop
// lands in, and the one it reports when the bootstrap never completes.
const (
	DBInstanceAvailable = "available"
	DBInstanceCreating  = "creating"
	DBInstanceModifying = "modifying"
	DBInstanceStopped   = "stopped"
	DBInstanceFailed    = "failed"

	// The only snapshot status a restore accepts.
	DBSnapshotAvailable = "available"
)

// Mirrors the tag handlers/rds stamps on the volumes and ENIs it creates, which
// is the only link from a DB identifier back to them. Duplicated rather than
// imported because the control plane keeps the key unexported.
const rdsDBInstanceTagKey = "spinifex:rds-db-instance"

// Per-target wait envelopes. Reaching available means the VM booted, the engine
// ran initdb and the in-guest agent reported healthy, so it is by far the
// longest; the rest are control-plane transitions with no bootstrap in them.
//
// available must outlast the control plane's own bootstrap timeout (20m,
// handlers/rds defaultBootstrapTimeout), or the two expire together and a
// wedged instance is reported as a bare wait expiry rather than as the failed
// status the reconciler was about to publish, with its reason attached.
//
// failed is bounded by the detection ladder rather than by a generous guess: a
// heartbeat goes stale after three intervals (90s), the classifier then holds a
// one-interval grace (30s), and the reconciler acts on its next pass (15s).
var dbInstanceWaitTimeouts = map[string]time.Duration{
	DBInstanceAvailable: 23 * time.Minute,
	DBInstanceFailed:    4 * time.Minute,
	DBInstanceStopped:   5 * time.Minute,
	DBInstanceModifying: 5 * time.Minute,
}

const (
	defaultDBInstanceWaitTimeout = 10 * time.Minute
	dbInstanceWaitInterval       = 10 * time.Second
)

// WaitForDBInstanceStatus polls DescribeDBInstances until the instance reports
// want, and fails fast when it reports failed instead of burning the envelope.
//
// It captures nothing on failure by design: CaptureDBDiagnostics is registered
// once per instance through OnFailure, so a t.Fatal here already produces the
// bundle without every wait signature having to carry a diagnostics argument.
func WaitForDBInstanceStatus(t *testing.T, c *AWSClient, id, want string, opts ...PollOpt) *rds.DBInstance {
	t.Helper()
	timeout, ok := dbInstanceWaitTimeouts[want]
	if !ok {
		timeout = defaultDBInstanceWaitTimeout
	}
	cfg := applyOpts(pollCfg{timeout: timeout, interval: dbInstanceWaitInterval}, opts...)
	var last *rds.DBInstance
	EventuallyErr(t, func() error {
		instance, err := DescribeDBInstance(c, id)
		if err != nil {
			return fmt.Errorf("describe-db-instances %s: %w", id, err)
		}
		last = instance
		status := aws.StringValue(instance.DBInstanceStatus)
		if status == want {
			return nil
		}
		// Terminal for any other target: the control plane has already given up
		// waiting for the engine, so polling on cannot change the answer.
		if status == DBInstanceFailed {
			t.Fatalf("DB instance %s entered %s, want %s", id, DBInstanceFailed, want)
		}
		return fmt.Errorf("%s status=%s want=%s", id, status, want)
	}, cfg.timeout, cfg.interval)
	t.Logf("DB instance %s reached status %s", id, want)
	return last
}

// WaitForDBInstanceAvailable waits for the status every create path ends in.
func WaitForDBInstanceAvailable(t *testing.T, c *AWSClient, id string, opts ...PollOpt) *rds.DBInstance {
	t.Helper()
	return WaitForDBInstanceStatus(t, c, id, DBInstanceAvailable, opts...)
}

// WaitForDBInstanceGone polls until the instance no longer exists. A group an
// instance still references cannot be deleted, and a deleting instance still
// counts, so a teardown that frees a group has to wait for the record to go.
func WaitForDBInstanceGone(t *testing.T, c *AWSClient, id string, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 10 * time.Minute, interval: dbInstanceWaitInterval}, opts...)
	EventuallyErr(t, func() error {
		instance, err := DescribeDBInstance(c, id)
		if ErrorCodeIs(err, "DBInstanceNotFound") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("describe-db-instances %s: %w", id, err)
		}
		return fmt.Errorf("%s still exists with status=%s", id, aws.StringValue(instance.DBInstanceStatus))
	}, cfg.timeout, cfg.interval)
	t.Logf("DB instance %s is gone", id)
}

// WaitForDBSnapshotAvailable polls until the snapshot reports available, which
// is the point the data exists and a restore will accept it.
func WaitForDBSnapshotAvailable(t *testing.T, c *AWSClient, id string, opts ...PollOpt) *rds.DBSnapshot {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 10 * time.Minute, interval: 5 * time.Second}, opts...)
	var last *rds.DBSnapshot
	EventuallyErr(t, func() error {
		snapshot, err := DescribeDBSnapshot(c, id)
		if err != nil {
			return fmt.Errorf("describe-db-snapshots %s: %w", id, err)
		}
		last = snapshot
		if status := aws.StringValue(snapshot.Status); status != DBSnapshotAvailable {
			return fmt.Errorf("%s status=%s want=%s", id, status, DBSnapshotAvailable)
		}
		return nil
	}, cfg.timeout, cfg.interval)
	t.Logf("DB snapshot %s reached status %s", id, DBSnapshotAvailable)
	return last
}

// DescribeDBInstance returns the single named DB instance. A named instance that
// does not exist is an error from the API, not an empty list.
func DescribeDBInstance(c *AWSClient, id string) (*rds.DBInstance, error) {
	out, err := c.RDS.DescribeDBInstances(&rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(id),
	})
	if err != nil {
		return nil, err
	}
	if len(out.DBInstances) != 1 {
		return nil, fmt.Errorf("describe %s returned %d instances, want 1", id, len(out.DBInstances))
	}
	return out.DBInstances[0], nil
}

// DescribeDBSnapshot returns the single named DB snapshot.
func DescribeDBSnapshot(c *AWSClient, id string) (*rds.DBSnapshot, error) {
	out, err := c.RDS.DescribeDBSnapshots(&rds.DescribeDBSnapshotsInput{
		DBSnapshotIdentifier: aws.String(id),
	})
	if err != nil {
		return nil, err
	}
	if len(out.DBSnapshots) != 1 {
		return nil, fmt.Errorf("describe %s returned %d snapshots, want 1", id, len(out.DBSnapshots))
	}
	return out.DBSnapshots[0], nil
}

// AgeAutomatedBackup rewinds the authoritative snapshot record so a live test
// can place a new backup beyond whole-day retention without waiting a day.
func AgeAutomatedBackup(t *testing.T, env *Env, accountID, snapshotID string, by time.Duration) {
	t.Helper()
	if by <= 0 {
		t.Fatalf("AgeAutomatedBackup %s: age must be positive, got %s", snapshotID, by)
	}

	host, token, ca := natsConn(t, env)
	nc, err := utils.ConnectNATS(host, token, ca)
	if err != nil {
		t.Fatalf("AgeAutomatedBackup %s: connect NATS %s: %v", snapshotID, host, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("AgeAutomatedBackup %s: jetstream context: %v", snapshotID, err)
	}
	kv, err := js.KeyValue(t.Context(), handlers_rds.AccountBucketName(accountID))
	if err != nil {
		t.Fatalf("AgeAutomatedBackup %s: open account KV: %v", snapshotID, err)
	}
	key := handlers_rds.DBSnapshotKey(snapshotID)
	entry, err := kv.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("AgeAutomatedBackup %s: read %s: %v", snapshotID, key, err)
	}

	var rec handlers_rds.DBSnapshotRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		t.Fatalf("AgeAutomatedBackup %s: decode %s: %v", snapshotID, key, err)
	}
	if rec.SnapshotType != handlers_rds.SnapshotTypeAutomated {
		t.Fatalf("AgeAutomatedBackup %s: snapshot type is %q, want %q",
			snapshotID, rec.SnapshotType, handlers_rds.SnapshotTypeAutomated)
	}
	if rec.CreatedAt.IsZero() {
		t.Fatalf("AgeAutomatedBackup %s: snapshot has no creation time", snapshotID)
	}
	rec.CreatedAt = rec.CreatedAt.Add(-by)
	payload, err := json.Marshal(&rec)
	if err != nil {
		t.Fatalf("AgeAutomatedBackup %s: encode record: %v", snapshotID, err)
	}
	if _, err := kv.Update(t.Context(), key, payload, entry.Revision()); err != nil {
		t.Fatalf("AgeAutomatedBackup %s: update %s: %v", snapshotID, key, err)
	}
	t.Logf("Aged automated backup %s by %s", snapshotID, by)
}

// ----------------------------------------------------------------------------
// The resources behind a DB instance
// ----------------------------------------------------------------------------

// DBInstanceVM returns the engine VM backing a DB instance.
//
// system must come from SystemAWSClient: the VM belongs to the system account
// and is filtered out of any other caller's DescribeInstances, so the tenant
// credentials that created the DB instance cannot see it at all.
func DBInstanceVM(t *testing.T, system *AWSClient, id string) *ec2.Instance {
	t.Helper()
	vmID, err := dbInstanceVMID(system, id)
	if err != nil {
		t.Fatalf("DBInstanceVM %s: %v", id, err)
	}
	if vmID == "" {
		t.Fatalf("DBInstanceVM %s: no attached data volume in the system account", id)
	}
	out, err := system.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(vmID)},
	})
	if err != nil {
		t.Fatalf("DBInstanceVM %s: describe %s: %v", id, vmID, err)
	}
	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		t.Fatalf("DBInstanceVM %s: VM %s not found in the system account", id, vmID)
	}
	return out.Reservations[0].Instances[0]
}

// DBInstanceDataVolume returns the data volume holding a DB instance's datadir.
// Kept separate from the boot volume so a replace can discard the VM and keep
// the data, which is what makes it the durable marker for the instance.
func DBInstanceDataVolume(t *testing.T, system *AWSClient, id string) *ec2.Volume {
	t.Helper()
	vols, err := dbInstanceVolumes(system, id)
	if err != nil {
		t.Fatalf("DBInstanceDataVolume %s: %v", id, err)
	}
	if len(vols) != 1 {
		t.Fatalf("DBInstanceDataVolume %s: found %d tagged volumes, want 1", id, len(vols))
	}
	return vols[0]
}

// DBEndpointENI returns the customer-facing ENI injected into the DB instance,
// read with the tenant credentials that own it. This is the ENI security groups
// are associated with, so it is where an SG-gating assertion has to look.
func DBEndpointENI(t *testing.T, tenant *AWSClient, id string) *ec2.NetworkInterface {
	t.Helper()
	enis, err := dbEndpointENIs(tenant, id)
	if err != nil {
		t.Fatalf("DBEndpointENI %s: %v", id, err)
	}
	if len(enis) != 1 {
		t.Fatalf("DBEndpointENI %s: found %d matching ENIs, want 1", id, len(enis))
	}
	return enis[0]
}

// dbInstanceVMID resolves the VM behind a DB instance through its data volume:
// no API exposes the internal instance ID, but the volume carries the DB
// identifier as a tag and names its own attachment. Empty when the volume is
// gone or unattached, which is a stopped or deleted instance rather than an error.
func dbInstanceVMID(system *AWSClient, id string) (string, error) {
	vols, err := dbInstanceVolumes(system, id)
	if err != nil {
		return "", err
	}
	if len(vols) != 1 {
		return "", fmt.Errorf("found %d volumes tagged %s=%s, want 1", len(vols), rdsDBInstanceTagKey, id)
	}
	for _, att := range vols[0].Attachments {
		if vmID := aws.StringValue(att.InstanceId); vmID != "" {
			return vmID, nil
		}
	}
	return "", nil
}

// dbInstanceVolumes lists the system-account volumes tagged for a DB instance.
// Volumes do support tag filters, which is why they — not the VM, which carries
// no per-instance tag — are the entry point for correlating one.
func dbInstanceVolumes(system *AWSClient, id string) ([]*ec2.Volume, error) {
	out, err := system.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:" + tags.ManagedByKey), Values: []*string{aws.String(tags.ManagedByRDS)}},
			{Name: aws.String("tag:" + rdsDBInstanceTagKey), Values: []*string{aws.String(id)}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe-volumes tagged %s=%s: %w", rdsDBInstanceTagKey, id, err)
	}
	return out.Volumes, nil
}

// DBSystemENI returns the DB VM's management NIC in the shared RDS system VPC,
// read with the system credentials that own it.
//
// This is the NIC no customer security group governs and no customer can see,
// which is exactly why an isolation assertion has to look at it: every DB VM in
// the deployment, across every account, has one in the same VPC.
func DBSystemENI(t *testing.T, system *AWSClient, id string) *ec2.NetworkInterface {
	t.Helper()
	out, err := system.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("description"),
			Values: []*string{aws.String("RDS management NIC for " + id)},
		}},
	})
	if err != nil {
		t.Fatalf("DBSystemENI %s: describe-network-interfaces: %v", id, err)
	}
	if len(out.NetworkInterfaces) != 1 {
		t.Fatalf("DBSystemENI %s: found %d matching ENIs, want 1", id, len(out.NetworkInterfaces))
	}
	return out.NetworkInterfaces[0]
}

// The prefix daemon namespaces its management-IP allocations under in the
// cluster-state bucket. Duplicated rather than imported because the daemon keeps
// the key unexported.
const mgmtIPAMKeyPrefix = "mgmt-ipam."

// DBInstanceMgmtIP resolves a DB VM's br-mgmt address from the cluster's
// management IPAM record.
//
// No API exposes it: the management NIC is host-local plumbing rather than a VPC
// interface. The runner sits on the same bridge, which makes this the one path
// from which a DB VM's engine port can be dialled without a customer VPC — and
// so the one place the bind can be proven from outside the guest.
func DBInstanceMgmtIP(t *testing.T, env *Env, system *AWSClient, id string) string {
	t.Helper()
	vm := DBInstanceVM(t, system, id)
	instanceID := aws.StringValue(vm.InstanceId)

	host, token, ca := natsConn(t, env)
	nc, err := utils.ConnectNATS(host, token, ca)
	if err != nil {
		t.Fatalf("DBInstanceMgmtIP %s: connect NATS %s: %v", id, host, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("DBInstanceMgmtIP %s: jetstream context: %v", id, err)
	}
	kv, err := js.KeyValue(t.Context(), daemon.ClusterStateBucket)
	if err != nil {
		t.Fatalf("DBInstanceMgmtIP %s: open %s: %v", id, daemon.ClusterStateBucket, err)
	}
	keys, err := kv.Keys(t.Context())
	if err != nil {
		t.Fatalf("DBInstanceMgmtIP %s: list %s: %v", id, daemon.ClusterStateBucket, err)
	}

	// br-mgmt can be more than one /24 across a cluster, so every allocation
	// record is scanned rather than the local node's.
	for _, key := range keys {
		if !strings.HasPrefix(key, mgmtIPAMKeyPrefix) {
			continue
		}
		entry, err := kv.Get(t.Context(), key)
		if err != nil {
			t.Fatalf("DBInstanceMgmtIP %s: read %s: %v", id, key, err)
		}
		var rec daemon.MgmtIPRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			t.Fatalf("DBInstanceMgmtIP %s: decode %s: %v", id, key, err)
		}
		for _, alloc := range rec.Allocated {
			if alloc.InstanceID == instanceID && alloc.IP != "" {
				t.Logf("DB instance %s VM %s has mgmt address %s", id, instanceID, alloc.IP)
				return alloc.IP
			}
		}
	}
	t.Fatalf("DBInstanceMgmtIP %s: VM %s holds no management IP allocation", id, instanceID)
	return ""
}

// dbEndpointENIs finds a DB instance's endpoint ENI in the tenant account. ENIs
// ignore tag filters, so the description the control plane sets at launch is the
// only discriminator available.
func dbEndpointENIs(tenant *AWSClient, id string) ([]*ec2.NetworkInterface, error) {
	out, err := tenant.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("description"),
			Values: []*string{aws.String("RDS endpoint ENI for " + id)},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("describe-network-interfaces for %s: %w", id, err)
	}
	return out.NetworkInterfaces, nil
}

// ----------------------------------------------------------------------------
// Diagnostics
// ----------------------------------------------------------------------------

// DBDiag is the client pair and output directory a DB-instance diagnostic
// bundle needs: the tenant credentials that own the DB instance record, the
// system-account credentials that own the VM behind it, and where to write.
type DBDiag struct {
	Tenant *AWSClient
	System *AWSClient
	Dir    string
}

// CaptureDBDiagnostics registers a failure-only bundle for one DB instance: the
// resolved describe payload, its events page, and the DB VM's serial console.
// Register it once, right after the create.
//
// Without it "create timed out" is indistinguishable between a slow AMI boot, a
// failed bootstrap fetch and an agent that never registered — three different
// owning phases. The console is the only window into the DB guest, which has no
// SSH by design.
func CaptureDBDiagnostics(t *testing.T, d DBDiag, id string) {
	t.Helper()
	OnFailure(t, func() {
		dumpDBInstanceDescribe(t, d, id)
		dumpDBInstanceEvents(t, d, id)

		vmID, err := dbInstanceVMID(d.System, id)
		if err != nil || vmID == "" {
			t.Logf("db diagnostics %s: no VM to capture a console from (%v)", id, err)
			return
		}
		DumpInstanceConsole(t, d.System, vmID, d.Dir, fmt.Sprintf("db-%s-console.log", id))
	})
}

func dumpDBInstanceDescribe(t *testing.T, d DBDiag, id string) {
	t.Helper()
	instance, err := DescribeDBInstance(d.Tenant, id)
	if err != nil {
		DumpFile(t, d.Dir, fmt.Sprintf("db-%s-describe.txt", id), []byte(err.Error()))
		return
	}
	payload, err := json.MarshalIndent(instance, "", "  ")
	if err != nil {
		t.Logf("db diagnostics %s: marshal describe: %v", id, err)
		return
	}
	DumpFile(t, d.Dir, fmt.Sprintf("db-%s-describe.json", id), payload)
}

// The events page carries the control plane's own account of what it attempted,
// which is the half the console cannot show.
func dumpDBInstanceEvents(t *testing.T, d DBDiag, id string) {
	t.Helper()
	out, err := d.Tenant.RDS.DescribeEvents(&rds.DescribeEventsInput{
		SourceIdentifier: aws.String(id),
		SourceType:       aws.String("db-instance"),
	})
	if err != nil {
		DumpFile(t, d.Dir, fmt.Sprintf("db-%s-events.txt", id), []byte(err.Error()))
		return
	}
	payload, err := json.MarshalIndent(out.Events, "", "  ")
	if err != nil {
		t.Logf("db diagnostics %s: marshal events: %v", id, err)
		return
	}
	DumpFile(t, d.Dir, fmt.Sprintf("db-%s-events.json", id), payload)
}

// ----------------------------------------------------------------------------
// Leak gates
// ----------------------------------------------------------------------------

// AssertNoRDSRemnants fails unless a deleted DB instance left neither its data
// volume in the system account nor its endpoint ENI in the tenant's. Teardown
// is asynchronous, so it polls rather than reading once, and a resource still
// mid-delete counts as present: only its absence proves teardown ran through.
//
// Two things it deliberately does not cover. The engine VM: it carries no
// per-instance tag, so it can only be asserted against an ID captured while the
// instance was alive — see AssertVMGone. And the DNS withdrawal: a record only
// proves anything resolved the way a customer resolves it, from inside a guest
// so that assertion belongs with ResolveInGuest.
func AssertNoRDSRemnants(t *testing.T, d DBDiag, id string, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 3 * time.Minute, interval: 5 * time.Second}, opts...)
	EventuallyErr(t, func() error {
		vols, err := dbInstanceVolumes(d.System, id)
		if err != nil {
			return err
		}
		if len(vols) > 0 {
			return fmt.Errorf("data volume %s still present (state=%s)",
				aws.StringValue(vols[0].VolumeId), aws.StringValue(vols[0].State))
		}
		enis, err := dbEndpointENIs(d.Tenant, id)
		if err != nil {
			return err
		}
		if len(enis) > 0 {
			return fmt.Errorf("endpoint ENI %s still present (status=%s)",
				aws.StringValue(enis[0].NetworkInterfaceId), aws.StringValue(enis[0].Status))
		}
		return nil
	}, cfg.timeout, cfg.interval)
	t.Logf("DB instance %s left no data volume or endpoint ENI behind", id)
}

// AssertVMGone polls until vmID reports terminated or has been purged from the
// catalog altogether. Both are the same outcome for a leak check, and a
// terminated VM is eventually dropped from DescribeInstances, so a wait that
// only accepts one of the two is a race.
func AssertVMGone(t *testing.T, c *AWSClient, vmID string, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 3 * time.Minute, interval: 5 * time.Second}, opts...)
	EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(vmID)},
		})
		if err != nil {
			if ErrorCodeIs(err, "InvalidInstanceID.NotFound") {
				return nil
			}
			return fmt.Errorf("describe-instances %s: %w", vmID, err)
		}
		if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
			return nil
		}
		state := aws.StringValue(out.Reservations[0].Instances[0].State.Name)
		if state == ec2.InstanceStateNameTerminated {
			return nil
		}
		return fmt.Errorf("VM %s still %s", vmID, state)
	}, cfg.timeout, cfg.interval)
	t.Logf("VM %s is gone", vmID)
}
