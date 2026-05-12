package daemon

import (
	"crypto/tls"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconcileOnHeal_NilJetStream — no jsManager ⇒ early return. Guard
// against panics in tests + single-node deployments where the manager is
// optional.
func TestReconcileOnHeal_NilJetStream(t *testing.T) {
	d := &Daemon{}
	d.reconcileOnHeal("test") // must not panic
	assert.Equal(t, uint64(0), d.Revision())
}

// TestReconcileOnHeal_BumpsRevision — jsManager non-nil ⇒ WriteState runs,
// stateRevision increments. Uses the same local-file path TestDaemonWriteState
// exercises so we don't need a live NATS server.
func TestReconcileOnHeal_BumpsRevision(t *testing.T) {
	d := &Daemon{
		config:    &config.Config{DataDir: t.TempDir()},
		vmMgr:     vm.NewManager(),
		jsManager: &JetStreamManager{}, // non-nil; WriteStateBytesBestEffort no-ops without a kv
	}
	d.vmMgr.Insert(&vm.VM{ID: "i-r1", InstanceType: "t3.micro"})

	d.reconcileOnHeal("test")
	assert.Equal(t, uint64(1), d.Revision())

	d.reconcileOnHeal("test")
	assert.Equal(t, uint64(2), d.Revision(), "second sequential call also runs once finished")
}

// TestReconcileOnHeal_CoalescesConcurrent — when reconciling is already set
// (i.e. a prior heal goroutine is mid-flight), a second invocation must
// observe the flag and return without re-running WriteState. Simulated by
// pre-setting the flag.
func TestReconcileOnHeal_Coalesces(t *testing.T) {
	d := &Daemon{
		config:    &config.Config{DataDir: t.TempDir()},
		vmMgr:     vm.NewManager(),
		jsManager: &JetStreamManager{},
	}
	d.reconciling.Store(true)

	d.reconcileOnHeal("test")

	assert.Equal(t, uint64(0), d.Revision(), "coalesced call must not run WriteState")
	assert.True(t, d.reconciling.Load(), "coalesced call must not clear the in-flight flag")
}

// TestReconcileOnHeal_ConcurrentRaceCoalesces — fire many goroutines and
// assert revision ≤ goroutine count. The exact final count depends on
// scheduling, but coalescing guarantees we never double-write inside a single
// overlapping window. The looser bound is enough — we assert no panics and
// at least one write succeeded.
func TestReconcileOnHeal_ConcurrentRaceCoalesces(t *testing.T) {
	d := &Daemon{
		config:    &config.Config{DataDir: t.TempDir()},
		vmMgr:     vm.NewManager(),
		jsManager: &JetStreamManager{},
	}

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for range N {
		go func() {
			defer wg.Done()
			<-start
			d.reconcileOnHeal("race")
		}()
	}
	close(start)
	wg.Wait()

	rev := d.Revision()
	assert.GreaterOrEqual(t, rev, uint64(1), "at least one heal must have run")
	assert.LessOrEqual(t, rev, uint64(N), "writes bounded by goroutine count")
	assert.False(t, d.reconciling.Load(), "flag must clear after race settles")
}

// TestProbePeersOnce_TriggersReconcileOnHeal — false→true edge in the peer
// probe must fire reconcileOnHeal (Scenario C: NATS client stays connected to
// loopback so onNATSReconnect is the wrong signal). Waits up to 1s for the
// goroutine to bump Revision.
func TestProbePeersOnce_TriggersReconcileOnHeal(t *testing.T) {
	f := newPeerHealthFixture(t, 1)
	d, _ := daemonForPeerHealth(t, f.cfg)
	d.config = &config.Config{DataDir: t.TempDir()}
	d.vmMgr = vm.NewManager()
	d.jsManager = &JetStreamManager{}

	client := &http.Client{
		Timeout:   peerProbeTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	peers := d.peerNodes()
	require.Len(t, peers, 1)

	// Start unreachable so the next probe is the false→true edge.
	f.peers[0].setHealthy(false)
	d.probePeersOnce(client, peers)
	require.False(t, d.peersReachable.Load())
	require.Equal(t, uint64(0), d.Revision())

	f.peers[0].setHealthy(true)
	d.probePeersOnce(client, peers)
	require.True(t, d.peersReachable.Load())

	require.Eventually(t, func() bool { return d.Revision() >= 1 },
		1*time.Second, 10*time.Millisecond,
		"reconcileOnHeal goroutine must run on peer-probe false→true edge")
}
