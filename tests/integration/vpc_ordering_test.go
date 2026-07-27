//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/subscribers"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// startVPCDEvents wires a real vpcd consumer over a real OVN NB DB with no
// daemon behind it. These tests publish the vpc.* events directly because the
// property under test is delivery order, which the SDK path cannot produce on
// demand: through it, CreateVpc necessarily precedes CreateSubnet.
func startVPCDEvents(t *testing.T) (*VPCDLite, *nats.Conn) {
	t.Helper()
	gw := StartGateway(t)
	return StartVPCDLite(t, gw), gw.NATSConn
}

// publishEvent marshals and fires a fire-and-forget vpc.* topology event, the
// way handlers/ec2/vpc's publishVPCEvent/publishSubnetEvent do.
func publishEvent(t *testing.T, nc *nats.Conn, topic string, evt any) {
	t.Helper()
	payload, err := json.Marshal(evt)
	require.NoError(t, err, "marshal %s event", topic)
	require.NoError(t, nc.Publish(topic, payload), "publish %s", topic)
	require.NoError(t, nc.Flush(), "flush %s", topic)
}

// countRouters returns how many logical routers carry name.
func countRouters(ctx context.Context, vpcd *VPCDLite, name string) (int, error) {
	routers, err := vpcd.OVN.ListLogicalRouters(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range routers {
		if r.Name == name {
			n++
		}
	}
	return n, nil
}

// TestVPCD_SubnetBeforeVPCConverges covers the case where vpc.create-subnet
// is delivered with no preceding vpc.create. The two topics have no ordering
// guarantee, so on new-tenant bootstrap the subnet can genuinely land first;
// the subnet handler's defensive pre-ensure is what stops that from being a
// dropped subnet.
//
// The convergence claim is the point: the pre-ensure creates the router
// without a CIDR, and the real vpc.create arriving afterwards must fill it in
// rather than being discarded as a duplicate or forking a second router.
func TestVPCD_SubnetBeforeVPCConverges(t *testing.T) {
	vpcd, nc := startVPCDEvents(t)
	ctx := context.Background()

	const vpcID, subnetID = "vpc-order-a", "subnet-order-a"
	router := topology.VPCRouter(vpcID)

	publishEvent(t, nc, subscribers.TopicSubnetCreate, subscribers.SubnetEvent{
		SubnetId:  subnetID,
		VpcId:     vpcID,
		CidrBlock: "10.50.1.0/24",
	})

	awaitNB(t, "logical switch for subnet-first delivery", func(ctx context.Context) error {
		_, err := vpcd.OVN.GetLogicalSwitch(ctx, topology.SubnetSwitch(subnetID))
		return err
	})

	publishEvent(t, nc, subscribers.TopicVPCCreate, subscribers.VPCEvent{
		VpcId:     vpcID,
		CidrBlock: "10.50.0.0/16",
	})

	awaitNB(t, "VPC CIDR backfilled onto pre-ensured router", func(ctx context.Context) error {
		lr, err := vpcd.OVN.GetLogicalRouter(ctx, router)
		if err != nil {
			return err
		}
		if got := lr.ExternalIDs["spinifex:cidr"]; got != "10.50.0.0/16" {
			return fmt.Errorf("spinifex:cidr = %q, want 10.50.0.0/16", got)
		}
		return nil
	})

	n, err := countRouters(ctx, vpcd, router)
	require.NoError(t, err, "list logical routers")
	require.Equal(t, 1, n, "out-of-order delivery forked %s into %d routers", router, n)
}
