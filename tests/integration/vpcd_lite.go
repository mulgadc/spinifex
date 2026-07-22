//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/subscribers"
	vpcdfixture "github.com/mulgadc/spinifex/tests/fixtures/vpcd"
	"github.com/stretchr/testify/require"
)

// VPCDLite is a real vpcd event consumer over a real in-process OVN
// Northbound DB. OVN is the connected client, for reading NB rows back.
type VPCDLite struct {
	OVN ovn.Client
}

// StartVPCDLite subscribes a real network/subscribers.Subscriber — the same
// production type a live vpcd runs — to every vpc.* lifecycle topic, backed by
// managers driving a genuine in-process OVN NB DB.
//
// This closes the one seam the tier otherwise leaves open. StartDaemonLite
// already runs the real VPCServiceImpl for the AWS-facing half, so an API call
// is authenticated, validated and persisted for real; but the vpc.* events it
// publishes had no consumer, so the SG topics were answered by a canned ack
// and the fire-and-forget topology topics were dropped. With this wired, an
// SDK call is asserted all the way through to OVN NB rows.
//
// Call BEFORE StartDaemonLite, and pass WithRealVPCD to it — see the ordering
// and double-responder notes on that option.
//
// What is deliberately absent: ovn-controller, so nothing translates NB to SB
// to OpenFlow, and no OVS bridge, tap, netns or nftables rule exists. Correct
// NB rows do not imply correct flows; that remains the live tier's claim.
func StartVPCDLite(t *testing.T, gw *Gateway) *VPCDLite {
	t.Helper()

	fixture := vpcdfixture.Start(t)

	subscriber, err := subscribers.New(fixture.Subscribers)
	require.NoError(t, err, "construct vpcd subscriber")

	subs, err := subscriber.Subscribe(gw.NATSConn)
	require.NoError(t, err, "subscribe vpcd topics")
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})

	return &VPCDLite{OVN: fixture.OVN}
}

// nbPollInterval/nbPollTimeout bound the wait in awaitNB. Topology events are
// fire-and-forget (utils.PublishEvent), so there is no reply to synchronise
// on: the only honest assertion is to poll until the row appears. The timeout
// is generous relative to an in-process OVSDB round-trip, since it is only
// ever paid in full by a genuine failure.
const (
	nbPollInterval = 10 * time.Millisecond
	nbPollTimeout  = 5 * time.Second
)

// awaitNB polls check until it returns nil, failing the test with the last
// error once nbPollTimeout elapses. Use it for anything driven by a
// fire-and-forget vpc.* event; the synchronous SG topics need no polling.
func awaitNB(t *testing.T, what string, check func(ctx context.Context) error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), nbPollTimeout)
	defer cancel()

	var lastErr error
	for {
		if lastErr = check(ctx); lastErr == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", what, lastErr)
		case <-time.After(nbPollInterval):
		}
	}
}
