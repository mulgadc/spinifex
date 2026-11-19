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

// The node reports level 2 only while its vpcd can capture and its gate serves
// endpoint proxies, and level 3 only while its vpcd names that build and its
// own prefix-list entry is the one every node derives.
func TestAdvertisedFeatureLevel_FollowsLocalVpcd(t *testing.T) {
	below := featurelevel.GatewayEndpoints - 1
	noPrefixList := featurelevel.SGPrefixList - 1
	advertised := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{"n1": {AdvertiseIP: "10.0.0.2"}}}
	unadvertised := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{
		"n1": {Predastore: config.PredastoreConfig{Host: "0.0.0.0:8443"}},
	}}
	ready := projection.EndpointProxyStatus{Node: "n1", ProbePassed: true, GateReady: true, Build: featurelevel.Build}
	with := func(f func(*projection.EndpointProxyStatus)) *projection.EndpointProxyStatus {
		st := ready
		f(&st)
		return &st
	}
	cases := []struct {
		name    string
		reply   *projection.EndpointProxyStatus
		cluster *config.ClusterConfig
		want    int
	}{
		{"ready", &ready, advertised, featurelevel.Build},
		{"no capture", with(func(s *projection.EndpointProxyStatus) { s.ProbePassed = false }), advertised, below},
		{"no gate listener", with(func(s *projection.EndpointProxyStatus) { s.GateReady = false }), advertised, below},
		{"another node answered", with(func(s *projection.EndpointProxyStatus) { s.Node = "n2" }), advertised, below},
		{"refused", with(func(s *projection.EndpointProxyStatus) { s.Error = "no" }), advertised, below},
		{"vpcd silent", nil, advertised, below},
		{"vpcd omits its build", with(func(s *projection.EndpointProxyStatus) { s.Build = 0 }), advertised, noPrefixList},
		{"vpcd below the prefix-list build", with(func(s *projection.EndpointProxyStatus) { s.Build = featurelevel.GatewayEndpoints }), advertised, noPrefixList},
		{"entry only at the bridge address", &ready, unadvertised, noPrefixList},
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
			d := &Daemon{node: "n1", natsConn: nc, clusterConfig: tc.cluster, mgmtBridgeIP: config.DefaultMgmtBridgeIP}
			assert.Equal(t, tc.want, d.advertisedFeatureLevel())
		})
	}
}

func TestAdvertisedFeatureLevel_NoNATSHoldsBelow(t *testing.T) {
	d := &Daemon{node: "n1"}
	assert.Equal(t, featurelevel.GatewayEndpoints-1, d.advertisedFeatureLevel())
}
