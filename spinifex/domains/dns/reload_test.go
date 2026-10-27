package dns_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/dns"
)

// ackAs subscribes a fake northstar that answers every reload with the acks
// reply returns for the requested zone.
func ackAs(t *testing.T, nc *nats.Conn, reply func(zone string) []dns.ZoneReloadAck) {
	t.Helper()
	sub, err := nc.Subscribe(dns.SubjectZoneReload, func(msg *nats.Msg) {
		var evt dns.ZoneReload
		require.NoError(t, json.Unmarshal(msg.Data, &evt))
		for _, ack := range reply(evt.Zone) {
			body, err := json.Marshal(ack)
			require.NoError(t, err)
			require.NoError(t, msg.Respond(body))
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func TestRequestZoneReloadCollectsOneAckPerServer(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	for _, node := range []string{"node1", "node2", "node3"} {
		ackAs(t, nc, func(zone string) []dns.ZoneReloadAck {
			return []dns.ZoneReloadAck{{Zone: zone, Node: node, ETag: "v2"}}
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acks, err := dns.RequestZoneReload(ctx, nc, "example.com", 3)
	require.NoError(t, err)
	require.Len(t, acks, 3)
	nodes := []string{acks[0].Node, acks[1].Node, acks[2].Node}
	assert.ElementsMatch(t, []string{"node1", "node2", "node3"}, nodes)
	for _, ack := range acks {
		assert.Equal(t, "v2", ack.ETag)
	}
}

func TestRequestZoneReloadIgnoresAcksForOtherZones(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	ackAs(t, nc, func(zone string) []dns.ZoneReloadAck {
		return []dns.ZoneReloadAck{{Zone: "other.com", Node: "stray"}, {Zone: zone, Node: "node1"}}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acks, err := dns.RequestZoneReload(ctx, nc, "example.com", 1)
	require.NoError(t, err)
	require.Len(t, acks, 1)
	assert.Equal(t, "node1", acks[0].Node)
}

func TestRequestZoneReloadReturnsPartialAcksAtDeadline(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	ackAs(t, nc, func(zone string) []dns.ZoneReloadAck {
		return []dns.ZoneReloadAck{{Zone: zone, Node: "node1", Error: "s3 unavailable"}}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	acks, err := dns.RequestZoneReload(ctx, nc, "example.com", 2)
	require.NoError(t, err)
	require.Len(t, acks, 1)
	assert.Equal(t, "s3 unavailable", acks[0].Error)
}

func TestRequestZoneReloadFailsOnClosedConnection(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	nc.Close()
	_, err := dns.RequestZoneReload(context.Background(), nc, "example.com", 1)
	require.Error(t, err)
}
