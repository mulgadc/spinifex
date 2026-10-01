//test:in-package — the start-path bucket openers and the constructors beneath
//them are unexported, and the point is which of the two waits.

package viperblockd

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// jetStreamArrivesAfter is how long the buckets are left failing before
// JetStream appears. Only long enough to prove the first refusal did not end
// the attempt; the retry interval decides how much longer the open then takes.
const jetStreamArrivesAfter = 250 * time.Millisecond

// TestVolumeBucketsWaitForJetStream is the cold multi-node start. Every service
// comes up at once against a cluster with no stream leader yet, and both of
// these opens are on the path to the daemon running at all — so a refusal that
// only means "not yet" must not become "this node has no block storage".
func TestVolumeBucketsWaitForJetStream(t *testing.T) {
	// Deliberately no JetStream: the server answers, it just has nowhere to put
	// a bucket, which is what a node whose cluster has not formed looks like.
	ns := natstest.RunServer(&server.Options{Host: "127.0.0.1", Port: -1})
	require.NotNil(t, ns)
	t.Cleanup(ns.Shutdown)
	clustersize.DeclareForTest(t, 1)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	leaseErr := make(chan error, 1)
	dirtyErr := make(chan error, 1)
	go func() { _, err := newVolumeLeasesWaiting(t.Context(), nc, "node-a"); leaseErr <- err }()
	go func() { _, err := newVolumeDirtyWaiting(t.Context(), nc, "node-a"); dirtyErr <- err }()

	select {
	case err := <-leaseErr:
		t.Fatalf("lease bucket concluded before JetStream existed: %v", err)
	case err := <-dirtyErr:
		t.Fatalf("dirty bucket concluded before JetStream existed: %v", err)
	case <-time.After(jetStreamArrivesAfter):
	}

	require.NoError(t, ns.EnableJetStream(&server.JetStreamConfig{StoreDir: t.TempDir()}))

	require.NoError(t, <-leaseErr, "the lease bucket must open once JetStream is there")
	require.NoError(t, <-dirtyErr, "the dirty bucket must open once JetStream is there")
}

// TestVolumeBucketsUnwaitingOpenStillRefusesAtOnce keeps the wait on the start
// path alone. The constructors are what the replica-count and cluster-size
// tests assert fail closed, and a refusal they have to wait out is one an
// operator tool and those tests would both read as a hang.
func TestVolumeBucketsUnwaitingOpenStillRefusesAtOnce(t *testing.T) {
	ns := natstest.RunServer(&server.Options{Host: "127.0.0.1", Port: -1})
	require.NotNil(t, ns)
	t.Cleanup(ns.Shutdown)
	clustersize.DeclareForTest(t, 1)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	started := time.Now()
	_, leaseOpenErr := newVolumeLeases(t.Context(), nc, "node-a")
	_, dirtyOpenErr := newVolumeDirty(t.Context(), nc, "node-a")

	require.Error(t, leaseOpenErr)
	require.Error(t, dirtyOpenErr)
	require.Less(t, time.Since(started), jetStreamArrivesAfter,
		"the plain constructors must report a refusal rather than retry it")
}
