//test:in-package — reads the unexported live set the orphan prune is built
// from, which no exported entry point returns.

package reconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
)

// The live set also gates the host-ingress sweep that prune runs, and a NAT
// gateway's address holds host state with no ENI and no dnat_and_snat row
// behind it. Built from ENI and user EIPs alone it read as absent from intent,
// so every drift pass tore down the ingress of every live NAT gateway.
func TestAddLiveCountsANATGatewayAddressAsLive(t *testing.T) {
	r, _ := newTestReconciler(t)

	intent := freshIntent(t)
	intent.NATGWs["nat-1"] = policy.NATGWSpec{
		VPCID: "vpc-a", NATGatewayID: "nat-1",
		PublicIP: "192.168.1.240", SubnetCIDR: "10.0.2.0/24",
	}

	live := policy.LiveEIPs{Ports: map[string]struct{}{}, ExternalIPs: map[string]struct{}{}}
	r.addLive(live, intent)

	assert.Contains(t, live.ExternalIPs, "192.168.1.240",
		"a live NAT gateway's address read as absent from intent")
}

// A released gateway is still swept: the set is intent, not history.
func TestAddLiveOmitsANATGatewayIntentNoLongerCarries(t *testing.T) {
	r, _ := newTestReconciler(t)

	live := policy.LiveEIPs{Ports: map[string]struct{}{}, ExternalIPs: map[string]struct{}{}}
	r.addLive(live, freshIntent(t))

	assert.NotContains(t, live.ExternalIPs, "192.168.1.240")
}
