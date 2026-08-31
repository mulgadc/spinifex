//test:in-package — these build *Daemon and *JetStreamManager literals to drive
//the desired-set builder against a seeded record store, which is only possible
//from inside the package.

package daemon

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dnsTestDaemon returns a daemon whose instance records live in a real KV
// bucket, plus a writer for seeding it. vmMgr is empty on purpose: the desired
// set must not read it.
func dnsTestDaemon(t *testing.T) (*Daemon, func(id, publicIP, privateIP string, state vm.InstanceState)) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	store := kvstore.New[vm.InstanceRecord](js, kvstore.Config{
		Name: InstanceStateBucket, History: 1, Replicas: 1,
	})
	d := &Daemon{
		jsManager:         &JetStreamManager{js: js, records: store, replicas: 1},
		vmMgr:             vm.NewManager(),
		config:            &config.Config{Region: "ap-southeast-2"},
		dnsBaseDomain:     "spx3.net",
		dnsInternalDomain: "compute.internal",
	}
	seed := func(id, publicIP, privateIP string, state vm.InstanceState) {
		t.Helper()
		record := &vm.InstanceRecord{}
		record.Metadata.Name = id
		record.Status.Status = state
		record.Status.PublicIP = publicIP
		if privateIP != "" {
			record.Status.Instance = &ec2.Instance{PrivateIpAddress: aws.String(privateIP)}
		}
		require.NoError(t, d.jsManager.WriteInstanceRecord(id, record))
	}
	return d, seed
}

func changeNames(changes []handlers_dns.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Name)
	}
	return out
}

// The desired set has to span every node, because that is the whole basis on
// which the reconcile is allowed to delete an instance record. A record written
// by another node is indistinguishable here from one written locally, which is
// the point.
func TestDesiredEC2DNSChanges_SpansEveryNode(t *testing.T) {
	d, seed := dnsTestDaemon(t)
	seed("i-local", "203.0.113.10", "172.31.0.10", vm.StateRunning)
	seed("i-remote", "203.0.113.20", "172.31.0.20", vm.StateRunning)

	changes, ok := d.desiredEC2DNSChanges()
	require.True(t, ok, "a successful list is a cluster-wide view and carries prune authority")
	assert.ElementsMatch(t, []string{
		"ec2-203-0-113-10.ap-southeast-2.compute.spx3.net",
		"ip-172-31-0-10.ap-southeast-2.compute.internal",
		"ec2-203-0-113-20.ap-southeast-2.compute.spx3.net",
		"ip-172-31-0-20.ap-southeast-2.compute.internal",
	}, changeNames(changes))
}

// The mutation check for the change itself: a VM present only in this node's
// in-memory manager must contribute nothing. Reading vmMgr is what made the
// view node-local and pruning unsafe, so a fallback to it would quietly restore
// the old behaviour while the prune authority claimed otherwise.
func TestDesiredEC2DNSChanges_IgnoresTheLocalVMManager(t *testing.T) {
	d, _ := dnsTestDaemon(t)
	d.vmMgr.Replace(map[string]*vm.VM{
		"i-inmemory": {
			ID:       "i-inmemory",
			Status:   vm.StateRunning,
			PublicIP: "203.0.113.99",
			Instance: &ec2.Instance{PrivateIpAddress: aws.String("172.31.0.99")},
		},
	})

	changes, ok := d.desiredEC2DNSChanges()
	require.True(t, ok)
	assert.Empty(t, changes, "an instance with no record is not part of the cluster-wide view")
}

// Only running instances resolve. A stopped instance keeps its record, so
// filtering on state is what stops its address resolving after it goes down.
func TestDesiredEC2DNSChanges_SkipsInstancesThatAreNotRunning(t *testing.T) {
	d, seed := dnsTestDaemon(t)
	seed("i-up", "203.0.113.10", "172.31.0.10", vm.StateRunning)
	seed("i-down", "203.0.113.11", "172.31.0.11", vm.StateStopped)

	changes, ok := d.desiredEC2DNSChanges()
	require.True(t, ok)
	assert.ElementsMatch(t, []string{
		"ec2-203-0-113-10.ap-southeast-2.compute.spx3.net",
		"ip-172-31-0-10.ap-southeast-2.compute.internal",
	}, changeNames(changes))
}

// A view that could not be read is the case pruning exists to be protected
// from. It must report no authority, and contribute no records either: repairing
// from a partial view is repairing from the same partial view.
func TestDesiredEC2DNSChanges_UnreadableStoreYieldsNoAuthority(t *testing.T) {
	for name, d := range map[string]*Daemon{
		"no jsManager":    {},
		"no record store": {jsManager: &JetStreamManager{}},
	} {
		t.Run(name, func(t *testing.T) {
			changes, ok := d.desiredEC2DNSChanges()
			assert.False(t, ok, "an unread instance view must not grant prune authority")
			assert.Empty(t, changes)
		})
	}
}

// The flag and the records travel together: a desired set carrying EC2 changes
// without EC2 authority would repair but never prune, and one carrying the flag
// without the changes would prune everything.
func TestDNSDesiredSet_EC2AuthorityFollowsTheRecords(t *testing.T) {
	d, seed := dnsTestDaemon(t)
	seed("i-up", "203.0.113.10", "172.31.0.10", vm.StateRunning)

	ds := d.dnsDesiredSet()
	assert.True(t, ds.Prunable.EC2)
	assert.NotEmpty(t, ds.Changes)

	assert.False(t, (&Daemon{}).dnsDesiredSet().Prunable.EC2,
		"a daemon that could not read any instance record claims no authority")
}
