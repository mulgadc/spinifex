// Package vpcd builds the network managers a live vpcd runs, wired against a
// real in-process OVN Northbound DB (network/ovn/ovntest). It exists so tests
// outside the network/ tree can assert against genuine NB rows instead of the
// hand-rolled ovn/mock.
//
// network/reconcile's scenario tests construct the same set of managers, but
// they do it inside a _test.go and so cannot be imported. This package
// deliberately repeats that wiring rather than exporting a helper out of
// network/: dependencies run tests/ -> spinifex/, and inverting that to save a
// block of construction is the worse trade.
package vpcd

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/ovntest"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/subscribers"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// connectTimeout bounds the initial OVSDB handshake. The server is in-process
// on a unix socket, so this only ever fires on a genuine failure.
const connectTimeout = 5 * time.Second

// Fixture is a connected OVN NB DB plus the managers that drive it. OVN is
// exposed so tests can read rows back; Subscribers is the config a
// subscribers.Subscriber is built from.
type Fixture struct {
	// OVN is the connected client, for asserting NB state directly.
	OVN ovn.Client
	// Subscribers carries every manager subscribers.New requires. MAC is left
	// nil: flushing SB MAC_Binding rows needs a real SB DB, which this fixture
	// does not run, and a nil flusher makes the flush a no-op.
	Subscribers subscribers.Config
}

// Start boots an in-process OVN NB server and returns the managers wired to
// it. The server and client are torn down via t.Cleanup.
//
// The managers are the production types a live vpcd constructs
// (spinifex/vpcd/vpcd.go), minus the host-dependent options: NAT runs in
// distributed mode with no chassis list, IGW allocates link-local addresses
// rather than drawing from a configured pool, and nothing seeds a nexthop MAC
// or waits on an OpenFlow barrier. Those all reach for the host datapath,
// which belongs to the live e2e tier.
func Start(t testing.TB) *Fixture {
	t.Helper()

	nb := ovntest.StartNB(t)

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	cli := ovn.NewLiveClient(nb.Endpoint)
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("vpcd fixture: LiveClient.Connect: %v", err)
	}
	t.Cleanup(cli.Close)

	sg := policy.NewSecurityGroupManager(cli)
	nat, err := policy.NewNATManager(cli, policy.NATModeDistributed)
	if err != nil {
		t.Fatalf("vpcd fixture: NewNATManager: %v", err)
	}
	routes := policy.NewRouteManager(cli)
	igw, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN:       cli,
		Routes:    routes,
		NAT:       nat,
		Allocator: external.LinkLocalAllocator{},
		NATMode:   policy.NATModeDistributed,
	})
	if err != nil {
		t.Fatalf("vpcd fixture: NewIGWManager: %v", err)
	}
	eip, err := external.NewEIPManager(nat, nil)
	if err != nil {
		t.Fatalf("vpcd fixture: NewEIPManager: %v", err)
	}
	natgw, err := external.NewNATGWManager(nat)
	if err != nil {
		t.Fatalf("vpcd fixture: NewNATGWManager: %v", err)
	}

	return &Fixture{
		OVN: cli,
		Subscribers: subscribers.Config{
			Topology: topology.NewLiveManager(cli),
			SG:       sg,
			EIP:      eip,
			NATGW:    natgw,
			IGW:      igw,
		},
	}
}
