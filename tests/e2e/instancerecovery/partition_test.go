//go:build e2e

package instancerecovery

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// How long the partitioned node is given to notice and stop its own guest.
	// Its lease validity is 30s and the check runs every 5s, so this is several
	// times what it should take; the value is generous because failing it means
	// a node kept writing to a volume it could not prove it owned, and that is
	// worth waiting for rather than guessing at.
	partitionFenceBudget = 4 * time.Minute

	// How long a survivor is given to claim and relaunch afterwards. The
	// server-side lease TTL has to expire on top of everything the ordinary
	// recovery waits for, so this is longer than recoveryBudget.
	partitionRecoveryBudget = 10 * time.Minute

	// How often the split-brain watch samples every node. Fast enough that a
	// second writer lasting one lease check would be seen.
	splitBrainPollInterval = 3 * time.Second

	// The fence and the surrender, as the node writes them. Matched because the
	// count of qemu processes says the guest stopped and these say why, and a
	// guest that stopped for another reason would prove nothing about fencing.
	partitionSurrenderLog = "volume lease could not be confirmed before its TTL, surrendering the volume"
	partitionFenceLog     = "fencing volume: the lease moved while this node had it open"
)

// runInstancePartitionRecovery is the production failure this whole feature
// exists for, and the only test here that produces it rather than approximating
// it with a service stop.
//
// A node loses its network to the rest of the cluster and nothing else. Its
// services stay up, its QEMU keeps running, its local NATS keeps answering its
// own clients — it is half healthy, and it cannot reach consensus. That is the
// state in which two nodes can each believe they own a volume, and the only
// thing standing between that and a corrupted disk is that the partitioned node
// gives up first, on a clock it can evaluate without reaching anything.
//
// So the assertion that matters is not that the guest comes back. It is that at
// no moment is it running in two places. The ordering the safety argument rests
// on — local validity (30s) lapses strictly before the server-side TTL (45s)
// lets anyone else in — is observable, and this is what observes it.
func runInstancePartitionRecovery(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Recovery From a Network Partition")
	recoveryWaitActive(t, fix)

	victim, instanceID := recoveryGuestOnAPeer(t, fix, diskLoadUserData)
	harness.Detail(t, "victim_node", victim.Name)
	harness.Detail(t, "instance", instanceID)

	// The guest has to be writing before the partition. A guest that never
	// touched its volume has no lease renewal to lose and nothing to fence, so
	// the test would pass having proved nothing.
	recoveryWaitForDiskLoad(t, fix, instanceID)

	cli := harness.AWSClientForGateway(t, fix.Env, fix.Cluster.Nodes[0])
	healthy := recoveryRequireStatus(t, cli, instanceID, func(s harness.InstanceStatusSummary) bool {
		return s.State == "running" && s.SystemStatus == "ok"
	}, 3*time.Minute, "state=running with system-status=ok before the partition")

	watch := newSplitBrainWatch(t, fix, instanceID)
	defer watch.stop()

	heal := harness.PartitionPeerPorts(t, fix.Cluster, victim, harness.NATSPorts)

	// Proving the node is still itself is what separates this from stopping it.
	// If its services had fallen over, the rest would be the ordinary recovery
	// test with extra steps.
	harness.Step(t, "confirm %s is still running its services and its guest", victim.Name)
	require.NotEmptyf(t, harness.InstanceHostingNodes(t, fix.Cluster, instanceID, nil),
		"%s is not running anywhere immediately after the partition, so the guest died for some other reason", instanceID)
	assert.Contains(t, recoveryRun(t, victim, "systemctl is-active spinifex-daemon spinifex-viperblock || true"),
		"active", "the partitioned node's services must still be up, or this is not a half-healthy node")

	harness.Step(t, "wait for %s to give up the volume lease it can no longer confirm and stop its own guest", victim.Name)
	require.Eventuallyf(t, func() bool {
		return len(harness.InstanceHostingNodes(t, fix.Cluster, instanceID, peerNames(fix, victim))) == 0
	}, partitionFenceBudget, 5*time.Second,
		"%s is still running %s after %s with no way to confirm it owns the volume; "+
			"a node that cannot prove it is the only writer and keeps writing is the corruption this fence exists to prevent\n%s",
		victim.Name, instanceID, partitionFenceBudget,
		lazyWhy(func() string { return partitionWhy(t, victim, instanceID) }))

	// Why it stopped, not just that it did. A guest killed by the OOM killer
	// would satisfy the count above and mean the opposite.
	journal := recoveryRun(t, victim, recoveryStorageJournalThisRun+" | grep -iE 'lease|fenc' | tail -40")
	assert.Containsf(t, journal, partitionSurrenderLog,
		"%s stopped the guest without recording that its lease went unconfirmed, so it stopped for some other reason\n%s",
		victim.Name, journal)
	assert.Contains(t, journal, partitionFenceLog,
		"the surrender has to reach the export: a lease given up while nbdkit keeps serving has fenced nothing")

	harness.Step(t, "wait for a survivor to take %s over", instanceID)
	var hosting []harness.Node
	require.Eventuallyf(t, func() bool {
		hosting = harness.InstanceHostingNodes(t, fix.Cluster, instanceID, []string{victim.Name})
		return len(hosting) > 0
	}, partitionRecoveryBudget, 15*time.Second,
		"%s never came back on a survivor while %s was partitioned\n%s",
		instanceID, victim.Name, lazyWhy(func() string { return recoveryWhy(t, fix, victim, instanceID) }))

	require.Lenf(t, hosting, 1, "%s is running on more than one survivor: %v",
		instanceID, recoveryNodeNames(hosting))
	harness.Detail(t, "recovered_onto", hosting[0].Name)

	recovered := recoveryRequireStatus(t, cli, instanceID, func(s harness.InstanceStatusSummary) bool {
		return s.State == "running" && s.SystemStatus == "ok" && s.InstanceStatus != "impaired"
	}, 5*time.Minute, "system-status back to ok once a survivor is running it")
	assert.Equal(t, healthy.AZ, recovered.AZ,
		"a recovery must not move an instance between availability zones")

	harness.Step(t, "heal the partition and assert %s does not bring the guest back", victim.Name)
	heal()
	fix.Cluster.WaitNATSPeers(t, len(fix.Cluster.Nodes)-1,
		harness.WithTimeout(3*time.Minute), harness.WithPoll(2*time.Second))

	// A rejoining node has a local state file that still lists the guest, and
	// nothing but the record stops it starting a second copy. Long enough that
	// it would have.
	time.Sleep(90 * time.Second)
	after := harness.InstanceHostingNodes(t, fix.Cluster, instanceID, nil)
	require.Lenf(t, after, 1,
		"%s is running on %v after the partition healed; a node that rejoins must drop a copy the records say moved",
		instanceID, recoveryNodeNames(after))
	assert.NotEqualf(t, victim.Name, after[0].Name,
		"%s went back to %s, so the rejoining node relaunched it rather than forgetting it",
		instanceID, victim.Name)

	// Last, because it covers the whole run including the heal. Nothing above
	// could have seen a second writer that appeared between its own samples.
	watch.stop()
	watch.requireNeverTwoWriters(t)
}

// splitBrainWatch samples which nodes are running a guest, continuously, for as
// long as a scenario lasts.
//
// The point-in-time assertions in the test above each answer "is it in two
// places now". This answers "was it ever", which is the question a corruption
// would be the consequence of, and the one no single sample can reach.
type splitBrainWatch struct {
	stopOnce sync.Once
	done     chan struct{}
	finished chan struct{}

	mu       sync.Mutex
	worst    []string
	samples  int
	failures int
}

func newSplitBrainWatch(t *testing.T, fix *Fixture, instanceID string) *splitBrainWatch {
	t.Helper()
	w := &splitBrainWatch{done: make(chan struct{}), finished: make(chan struct{})}

	go func() {
		defer close(w.finished)
		ticker := time.NewTicker(splitBrainPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				hosting := harness.InstanceHostingNodes(t, fix.Cluster, instanceID, nil)
				w.mu.Lock()
				w.samples++
				if len(hosting) > 1 {
					w.failures++
					if len(hosting) > len(w.worst) {
						w.worst = recoveryNodeNames(hosting)
					}
				}
				w.mu.Unlock()
			}
		}
	}()
	return w
}

func (w *splitBrainWatch) stop() {
	w.stopOnce.Do(func() {
		close(w.done)
		<-w.finished
	})
}

// requireNeverTwoWriters is the assertion the scenario exists for. A single
// sample naming two nodes is a failure — the guests write to the same volume
// through the same object store, so two of them is data loss already under way,
// not a race that might resolve.
func (w *splitBrainWatch) requireNeverTwoWriters(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()

	require.Positivef(t, w.samples,
		"the split-brain watch took no samples, so it proved nothing about two writers")
	harness.Detail(t, "split_brain_samples", fmt.Sprintf("%d", w.samples))
	require.Zerof(t, w.failures,
		"the instance was running on more than one node in %d of %d samples, worst case %v; "+
			"two guests writing one volume through one object store is data loss already under way",
		w.failures, w.samples, w.worst)
}

// peerNames is every node but victim, for a poll that must ask only the victim.
func peerNames(fix *Fixture, victim harness.Node) []string {
	var out []string
	for _, node := range fix.Cluster.Nodes {
		if node.Name != victim.Name {
			out = append(out, node.Name)
		}
	}
	return out
}

// partitionWhy gathers the partitioned node's own account, for a failure that
// would otherwise send the reader to a journal on a node they have to remember
// is partitioned.
func partitionWhy(t *testing.T, victim harness.Node, instanceID string) string {
	t.Helper()
	var b strings.Builder
	for _, query := range []string{
		recoveryJournalThisRun + " | grep -iE 'lease|fenc|nats' | tail -30",
		recoveryStorageJournalThisRun + " | grep -iE 'lease|fenc|nats' | tail -30",
		"ps auxw | grep -F " + harness.ShellQuote(instanceID) + " | grep -v grep || true",
		"sudo nft list table inet " + "spx_e2e_partition" + " 2>&1 | head -30",
	} {
		out, err := recoveryRunErr(victim, query)
		if err != nil {
			out = "(" + err.Error() + ")"
		}
		fmt.Fprintf(&b, "         %s: %s\n%s\n", victim.Name, query, strings.TrimSpace(out))
	}
	return b.String()
}
