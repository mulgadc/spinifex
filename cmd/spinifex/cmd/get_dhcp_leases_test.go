package cmd

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLeaseNodeLabel(t *testing.T) {
	tests := []struct {
		vendorClass string
		want        string
	}{
		{vendorClass: "spinifex-vpcd/node1", want: "node1"},
		{vendorClass: "spinifex-vpcd", want: "-"},
		{vendorClass: "", want: "-"},
	}

	for _, tt := range tests {
		got := leaseNodeLabel(dhcp.Entry{Lease: &dhcp.Lease{VendorClass: tt.vendorClass}})
		assert.Equal(t, tt.want, got, "vendor class %q", tt.vendorClass)
	}
}

func TestFormatLeaseExpiry(t *testing.T) {
	// A lease the server has already aged out is the one an operator needs to
	// spot, so it reads as a word rather than a negative duration.
	aged := dhcp.Entry{Lease: &dhcp.Lease{
		AcquiredAt:    time.Now().Add(-2 * time.Hour),
		LeaseDuration: time.Hour,
	}}
	assert.Equal(t, "expired", formatLeaseExpiry(aged))

	live := dhcp.Entry{Lease: &dhcp.Lease{
		AcquiredAt:    time.Now(),
		LeaseDuration: 30 * time.Minute,
	}}
	assert.Contains(t, formatLeaseExpiry(live), "m")

	assert.Equal(t, "-", formatLeaseExpiry(dhcp.Entry{Lease: &dhcp.Lease{}}))
}

func TestListDHCPLeasesReadsEveryAZBucket(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	for az, clientIDs := range map[string][]string{
		"az2": {"eipalloc-2"},
		"az1": {"eipalloc-1", "dhcp-gw-lrp-vpc-1"},
	} {
		store, err := dhcp.NewStore(t.Context(), js, az)
		require.NoError(t, err)
		for _, id := range clientIDs {
			require.NoError(t, store.Put(t.Context(), dhcp.Entry{
				Purpose: dhcp.PurposeEIP,
				Lease:   &dhcp.Lease{ClientID: id, IP: net.ParseIP("192.168.1.5")},
			}))
		}
	}

	entries, err := listDHCPLeases(t.Context(), nc)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	// Sorted by AZ then client-id, so the same cluster always prints the same way.
	assert.Equal(t, []string{"az1", "az1", "az2"}, []string{entries[0].az, entries[1].az, entries[2].az})
	assert.Equal(t, "dhcp-gw-lrp-vpc-1", entries[0].entry.Lease.ClientID)
	assert.Equal(t, "eipalloc-1", entries[1].entry.Lease.ClientID)
	assert.Equal(t, "eipalloc-2", entries[2].entry.Lease.ClientID)
}

func TestListDHCPLeasesWithoutBuckets(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	entries, err := listDHCPLeases(t.Context(), nc)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestDHCPLeaseOwnerStatus(t *testing.T) {
	tests := []struct {
		name  string
		reply any
		want  string
	}{
		{name: "alive", reply: dhcp.OwnerCheckReply{Status: dhcp.OwnerStatusAlive}, want: "alive"},
		{name: "gone", reply: dhcp.OwnerCheckReply{Status: dhcp.OwnerStatusGone}, want: "gone"},
		{
			name:  "lookup failed",
			reply: dhcp.OwnerCheckReply{Status: dhcp.OwnerStatusUnknown, Error: "kv timeout"},
			want:  "unknown",
		},
		// An unparseable reply says nothing about the resource, so it cannot read
		// as gone — that would have the reaper release a live address.
		{name: "garbage reply", reply: "not-a-reply", want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, nc, _ := testutil.StartTestJetStream(t)
			payload := []byte("not json")
			if reply, ok := tt.reply.(dhcp.OwnerCheckReply); ok {
				var err error
				payload, err = json.Marshal(reply)
				require.NoError(t, err)
			}
			sub, err := nc.Subscribe(dhcp.TopicOwnerCheck, func(msg *nats.Msg) {
				_ = msg.Respond(payload)
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = sub.Unsubscribe() })

			entry := dhcp.Entry{Purpose: dhcp.PurposeEIP, Lease: &dhcp.Lease{ClientID: "eipalloc-1"}}
			assert.Equal(t, tt.want, dhcpLeaseOwnerStatus(t.Context(), nc, entry, time.Second))
		})
	}
}

// No daemon answering means the lease's owner is unproven, not gone.
func TestDHCPLeaseOwnerStatusWithoutResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	entry := dhcp.Entry{Purpose: dhcp.PurposeEIP, Lease: &dhcp.Lease{ClientID: "eipalloc-1"}}
	assert.Equal(t, "unknown", dhcpLeaseOwnerStatus(t.Context(), nc, entry, time.Second))
}
