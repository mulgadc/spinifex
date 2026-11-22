package daemon

import (
	"encoding/json"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The node reports level 2 only while its vpcd can capture and names that
// build, its gate serves endpoint proxies and enforces their policies, and its
// own prefix-list entry is the one every node derives.
func TestAdvertisedFeatureLevel_FollowsLocalVpcd(t *testing.T) {
	below := featurelevel.GatewayEndpoints - 1
	advertised := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{"n1": {AdvertiseIP: "10.0.0.2"}}}
	unadvertised := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{
		"n1": {Predastore: config.PredastoreConfig{Host: "0.0.0.0:8443"}},
	}}
	reboundGateway := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{
		"n1": {AdvertiseIP: "10.0.0.2", AWSGW: config.AWSGWConfig{Host: "10.0.0.9:9999"}},
	}}
	peerOnly := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{
		"n1": {}, "n2": {AdvertiseIP: "10.0.0.3"},
	}}
	ready := projection.EndpointProxyStatus{Node: "n1", ProbePassed: true, GateReady: true, GateContract: 2, Build: featurelevel.Build}
	with := func(f func(*projection.EndpointProxyStatus)) *projection.EndpointProxyStatus {
		st := ready
		f(&st)
		return &st
	}
	cases := []struct {
		name    string
		reply   *projection.EndpointProxyStatus
		cluster *config.ClusterConfig
		mgmt    string
		want    int
	}{
		{"ready", &ready, advertised, config.DefaultMgmtBridgeIP, featurelevel.Build},
		{"no capture", with(func(s *projection.EndpointProxyStatus) { s.ProbePassed = false }), advertised, config.DefaultMgmtBridgeIP, below},
		{"no gate listener", with(func(s *projection.EndpointProxyStatus) { s.GateReady = false }), advertised, config.DefaultMgmtBridgeIP, below},
		{"gate without endpoint policies", with(func(s *projection.EndpointProxyStatus) { s.GateContract = 1 }), advertised, config.DefaultMgmtBridgeIP, below},
		{"vpcd omits the gate contract", with(func(s *projection.EndpointProxyStatus) { s.GateContract = 0 }), advertised, config.DefaultMgmtBridgeIP, below},
		{"another node answered", with(func(s *projection.EndpointProxyStatus) { s.Node = "n2" }), advertised, config.DefaultMgmtBridgeIP, below},
		{"refused", with(func(s *projection.EndpointProxyStatus) { s.Error = "no" }), advertised, config.DefaultMgmtBridgeIP, below},
		{"vpcd silent", nil, advertised, config.DefaultMgmtBridgeIP, below},
		{"vpcd omits its build", with(func(s *projection.EndpointProxyStatus) { s.Build = 0 }), advertised, config.DefaultMgmtBridgeIP, below},
		{"vpcd reports an older build", with(func(s *projection.EndpointProxyStatus) { s.Build = featurelevel.Baseline }), advertised, config.DefaultMgmtBridgeIP, below},
		{"entry only at the bridge address", &ready, unadvertised, config.DefaultMgmtBridgeIP, below},
		{"no entry of its own in either derivation", &ready, peerOnly, "", below},
		{"gateway bound away from its advertise address", &ready, reboundGateway, config.DefaultMgmtBridgeIP, below},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, nc := testutil.StartTestNATS(t)
			if tc.reply != nil {
				sub, err := nc.Subscribe(projection.EndpointProxyStatusSubject("n1"), func(m *nats.Msg) {
					data, _ := json.Marshal(tc.reply)
					_ = m.Respond(data)
				})
				require.NoError(t, err)
				t.Cleanup(func() { _ = sub.Unsubscribe() })
			}
			d := &Daemon{node: "n1", natsConn: nc, clusterConfig: tc.cluster, mgmtBridgeIP: tc.mgmt}
			assert.Equal(t, tc.want, d.advertisedFeatureLevel())
		})
	}
}

func TestAdvertisedFeatureLevel_NoNATSHoldsBelow(t *testing.T) {
	d := &Daemon{node: "n1"}
	assert.Equal(t, featurelevel.GatewayEndpoints-1, d.advertisedFeatureLevel())
}

// The demux matches the gossiped derivation while level 2 holds, so a node
// rebound after activation demuxes the addresses vpcd captures.
func TestEndpointEntries_GossipedWhileLevel2Holds(t *testing.T) {
	t.Parallel()
	_, store := levelManager(t)
	cc := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{
		"n1": {Predastore: config.PredastoreConfig{Host: "0.0.0.0:8443"}},
	}}
	d := &Daemon{node: "n1", clusterConfig: cc, mgmtBridgeIP: config.DefaultMgmtBridgeIP, featureGate: featurelevel.NewGate(store)}
	local := d.endpointEntries(t.Context())
	require.NotEmpty(t, local, "below level 2 the node's own bridge entry is listed")

	_, _, err := store.Advance(t.Context(), []featurelevel.Member{{Node: "n1", Level: featurelevel.GatewayEndpoints, Current: true}})
	require.NoError(t, err)
	assert.Empty(t, d.endpointEntries(t.Context()), "level 2 lists only gossiped addresses")
}

// Prefix-list writes are admitted, and reads switch to the gossiped
// derivation, only while level 2 is active: not at 1, nor once a rollback
// below 2 is recorded.
func TestPrefixListActivation_FollowsLevel2(t *testing.T) {
	t.Parallel()
	_, store := levelManager(t)
	gate := featurelevel.NewGate(store)
	admit, gossiped := prefixListAdmission(gate), prefixListGossiped(gate)
	assertActive := func(want bool, msg string) {
		t.Helper()
		done, ok, err := admit(t.Context())
		require.NoError(t, err, msg)
		done()
		assert.Equal(t, want, ok, msg)
		assert.Equal(t, want, gossiped(t.Context()), msg)
	}

	assertActive(false, "level 1")
	_, _, err := store.Advance(t.Context(), []featurelevel.Member{{Node: "n1", Level: featurelevel.GatewayEndpoints, Current: true}})
	require.NoError(t, err)
	assertActive(true, "level 2")
	_, _, err = store.StartRollback(t.Context(), featurelevel.GatewayEndpoints-1, featurelevel.Build)
	require.NoError(t, err)
	assertActive(false, "rollback below 2 recorded")

	_, ok, err := prefixListAdmission(nil)(t.Context())
	require.Error(t, err, "no gate yet")
	assert.False(t, ok)
	assert.False(t, prefixListGossiped(nil)(t.Context()))
}
