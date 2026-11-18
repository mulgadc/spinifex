package daemon

import (
	"encoding/json"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The node reports the gateway-endpoint level only while its vpcd says it can
// capture and its gate serves endpoint proxies; anything less holds it below.
func TestAdvertisedFeatureLevel_FollowsLocalVpcd(t *testing.T) {
	below := featurelevel.GatewayEndpoints - 1
	cases := []struct {
		name  string
		reply *projection.EndpointProxyStatus
		want  int
	}{
		{"ready", &projection.EndpointProxyStatus{Node: "n1", ProbePassed: true, GateReady: true}, featurelevel.Build},
		{"no capture", &projection.EndpointProxyStatus{Node: "n1", GateReady: true}, below},
		{"no gate listener", &projection.EndpointProxyStatus{Node: "n1", ProbePassed: true}, below},
		{"another node answered", &projection.EndpointProxyStatus{Node: "n2", ProbePassed: true, GateReady: true}, below},
		{"refused", &projection.EndpointProxyStatus{Node: "n1", ProbePassed: true, GateReady: true, Error: "no"}, below},
		{"vpcd silent", nil, below},
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
			d := &Daemon{node: "n1", natsConn: nc}
			assert.Equal(t, tc.want, d.advertisedFeatureLevel())
		})
	}
}

func TestAdvertisedFeatureLevel_NoNATSHoldsBelow(t *testing.T) {
	d := &Daemon{node: "n1"}
	assert.Equal(t, featurelevel.GatewayEndpoints-1, d.advertisedFeatureLevel())
}
