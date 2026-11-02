package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// answerNodeStatus replies to the node-status fan-out as the given nodes would.
func answerNodeStatus(t *testing.T, nc *nats.Conn, nodes ...clusterv1.NodeStatusResponse) {
	t.Helper()
	for _, node := range nodes {
		data, err := json.Marshal(node)
		require.NoError(t, err)
		sub, err := nc.Subscribe(clusterv1.NodeStatusSubject, func(m *nats.Msg) {
			_ = m.Respond(data)
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	require.NoError(t, nc.Flush())
}

func node(name string, types ...clusterv1.InstanceTypeCap) clusterv1.NodeStatusResponse {
	return clusterv1.NodeStatusResponse{Node: name, InstanceTypes: types}
}

func TestGetInstanceTypeCapacitySumsAcrossNodes(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc,
		node("node1", clusterv1.InstanceTypeCap{Name: "t3.micro", Available: 60}, clusterv1.InstanceTypeCap{Name: "c6a.large", Available: 4}),
		node("node2", clusterv1.InstanceTypeCap{Name: "t3.micro", Available: 62}),
	)

	out, err := GetInstanceTypeCapacity(context.Background(), nc, 2, "000000000002")

	require.NoError(t, err)
	assert.Equal(t, int64(122), out.Available["t3.micro"])
	assert.Equal(t, int64(4), out.Available["c6a.large"])
	assert.Equal(t, 2, out.Nodes)
}

// The reply carries a count per type, so its size follows the catalogue and not
// how much capacity is free. This is the property the EC2 listing lost.
func TestGetInstanceTypeCapacityReplyIsBoundedByTypeCount(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc, node("node1",
		clusterv1.InstanceTypeCap{Name: "t3.micro", Available: 40000},
		clusterv1.InstanceTypeCap{Name: "t3.small", Available: 20000},
	))

	out, err := GetInstanceTypeCapacity(context.Background(), nc, 1, "000000000002")
	require.NoError(t, err)

	data, err := json.Marshal(out)
	require.NoError(t, err)

	assert.Len(t, out.Available, 2)
	assert.Less(t, len(data), 256, "60,000 free slots must not grow the reply: %s", data)
}

// A type the cluster supports but cannot currently admit is absent rather than
// reported as zero, so a client reads one answer and not two.
func TestGetInstanceTypeCapacityOmitsTypesWithNoFreeSlot(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc, node("node1",
		clusterv1.InstanceTypeCap{Name: "t3.micro", Available: 3},
		clusterv1.InstanceTypeCap{Name: "x2gd.metal", Available: 0},
	))

	out, err := GetInstanceTypeCapacity(context.Background(), nc, 1, "000000000002")

	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"t3.micro": 3}, out.Available)
}

// A node answering twice would double the cluster's apparent capacity.
func TestGetInstanceTypeCapacityCountsEachNodeOnce(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc,
		node("node1", clusterv1.InstanceTypeCap{Name: "t3.micro", Available: 5}),
		node("node1", clusterv1.InstanceTypeCap{Name: "t3.micro", Available: 5}),
	)

	out, err := GetInstanceTypeCapacity(context.Background(), nc, 2, "000000000002")

	require.NoError(t, err)
	assert.Equal(t, int64(5), out.Available["t3.micro"])
	assert.Equal(t, 1, out.Nodes)
}

// Capacity is not operator-only: the launch form a tenant uses is what needs
// the figure, so this action must not be account-gated the way GetNodes is.
func TestRegistration_InstanceTypeCapacityIsNotAdminOnly(t *testing.T) {
	assert.False(t, adminActions[GetInstanceTypeCapacityAction])
	assert.NotContains(t, AdminActionNames(), GetInstanceTypeCapacityAction)

	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc, node("node1", clusterv1.InstanceTypeCap{Name: "t3.micro", Available: 7}))

	w, err := serve(t, Deps{NATS: nc, NodeIDs: []string{"node1"}}, "000000000002",
		action(GetInstanceTypeCapacityAction), nil)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"t3.micro":7`)
}

// The action is reachable for the super-admin account too, so the operator
// console and a tenant console read the same figure.
func TestRegistration_InstanceTypeCapacityServesTheAdminAccount(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	answerNodeStatus(t, nc, node("node1", clusterv1.InstanceTypeCap{Name: "t3.micro", Available: 7}))

	w, err := serve(t, Deps{NATS: nc, NodeIDs: []string{"node1"}}, awsidentifiers.DefaultAccountID(),
		action(GetInstanceTypeCapacityAction), nil)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)
}

// The inventory feeds the coverage surface and is not cross-checked against the
// dispatcher, so an action added to one and not the other fails nothing.
func TestRegistration_InventoryNamesEveryDispatchedAction(t *testing.T) {
	assert.Contains(t, ActionNames(), GetInstanceTypeCapacityAction)
	assert.Contains(t, ActionNames(), GetInstanceMetricsAction)

	for _, name := range AdminActionNames() {
		assert.Contains(t, ActionNames(), name)
	}

	// A dispatched action absent from the inventory would still answer, so the
	// check runs the other way too: every registered name must dispatch.
	for _, name := range ActionNames() {
		t.Run(name, func(t *testing.T) {
			_, err := serve(t, Deps{}, awsidentifiers.DefaultAccountID(), action(name), nil)
			if err == nil {
				return
			}
			assert.NotContains(t, err.Error(), "InvalidAction",
				"%s is in the inventory but the dispatcher does not know it", name)
		})
	}
}
