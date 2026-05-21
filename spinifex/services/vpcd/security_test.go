package vpcd

// Phase 2.6 (mulga-siv-129) tests for the SG NATS wrappers in security.go.
// ACL emission semantics live in network/policy/sg_test.go +
// network/policy/acl_test.go; tests here cover the wrapper layer:
// event-shape errors, OVN-client wiring, and end-to-end NATS → OVN happy
// paths through the new managers.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sgTopoFixture(t *testing.T) (*TopologyHandler, *MockOVNClient) {
	t.Helper()
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	return NewTopologyHandler(mock), mock
}

func TestSG_CreateSG_HappyPath(t *testing.T) {
	_, nc := startTestNATS(t)
	topo, mock := sgTopoFixture(t)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	evt := sgEvent{GroupId: "sg-create1", VpcId: "vpc-test"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG")

	mock.Mu.Lock()
	_, pgExists := mock.PortGroups["sg_create1"]
	aclCount := len(mock.ACLs)
	mock.Mu.Unlock()
	assert.True(t, pgExists, "port group sg_create1 must exist after create-sg")
	assert.Equal(t, 4, aclCount, "infra ACL set (2 deny + 2 DHCP) must be attached")
}

func TestSG_CreateSG_Idempotent(t *testing.T) {
	_, nc := startTestNATS(t)
	topo, mock := sgTopoFixture(t)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(sgEvent{GroupId: "sg-idem", VpcId: "vpc-test"})
	for range 2 {
		resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
		require.NoError(t, err)
		assertSuccess(t, resp, "create SG retry")
	}

	mock.Mu.Lock()
	pgCount := len(mock.PortGroups)
	mock.Mu.Unlock()
	assert.Equal(t, 1, pgCount, "duplicate create-sg must not create a second port group")
}

func TestSG_DeleteSG_HappyPath(t *testing.T) {
	_, nc := startTestNATS(t)
	topo, mock := sgTopoFixture(t)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(sgEvent{GroupId: "sg-del1", VpcId: "vpc-test"})
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG before delete")

	resp, err = nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "delete SG")

	mock.Mu.Lock()
	_, pgExists := mock.PortGroups["sg_del1"]
	mock.Mu.Unlock()
	assert.False(t, pgExists, "port group must be gone after delete-sg")
}

// Delete of a never-created SG must succeed: the new wrapper delegates to
// topology.Manager.DeleteSGPortGroup which is idempotent on absent port
// groups (the reconciler retries delete events that race with vm-cleanup).
func TestSG_DeleteSG_IdempotentOnAbsent(t *testing.T) {
	_, nc := startTestNATS(t)
	topo, _ := sgTopoFixture(t)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(sgEvent{GroupId: "sg-ghost", VpcId: "vpc-test"})
	resp, err := nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "delete absent SG must be a no-op")
}

func TestSG_UpdateSG_ReplacesACLSet(t *testing.T) {
	_, nc := startTestNATS(t)
	topo, mock := sgTopoFixture(t)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	createData, _ := json.Marshal(sgEvent{GroupId: "sg-upd", VpcId: "vpc-test"})
	resp, err := nc.Request(TopicCreateSG, createData, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG before update")

	updateData, _ := json.Marshal(sgEvent{
		GroupId: "sg-upd",
		VpcId:   "vpc-test",
		IngressRules: []sgRule{
			{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/8"},
		},
	})
	resp, err = nc.Request(TopicUpdateSG, updateData, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "update SG")

	mock.Mu.Lock()
	pg := mock.PortGroups["sg_upd"]
	aclCount := 0
	hasTenantRule := false
	if pg != nil {
		aclCount = len(pg.ACLs)
		for _, uuid := range pg.ACLs {
			if acl := mock.ACLs[uuid]; acl != nil && acl.Priority == 1000 {
				hasTenantRule = true
			}
		}
	}
	mock.Mu.Unlock()
	assert.Equal(t, 5, aclCount, "update must leave 4 infra + 1 tenant ACL on the port group")
	assert.True(t, hasTenantRule, "tenant priority-1000 ACL must be present after update")
}

func TestSG_HandleCreateSG_OVNNotConnected(t *testing.T) {
	_, nc := startTestNATS(t)
	topo := NewTopologyHandler(nil)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(sgEvent{GroupId: "sg-noovn"})
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "create SG must fail when ovn client is nil")
}

func TestSG_HandleDeleteSG_OVNNotConnected(t *testing.T) {
	_, nc := startTestNATS(t)
	topo := NewTopologyHandler(nil)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(sgEvent{GroupId: "sg-noovn"})
	resp, err := nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "delete SG must fail when ovn client is nil")
}

func TestSG_HandleUpdateSG_OVNNotConnected(t *testing.T) {
	_, nc := startTestNATS(t)
	topo := NewTopologyHandler(nil)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(sgEvent{GroupId: "sg-noovn"})
	resp, err := nc.Request(TopicUpdateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "update SG must fail when ovn client is nil")
}

func TestSG_HandleCreateSG_BadJSON(t *testing.T) {
	_, nc := startTestNATS(t)
	topo, _ := sgTopoFixture(t)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	resp, err := nc.Request(TopicCreateSG, []byte("not json"), 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "create SG with malformed payload must fail")
}

func TestSG_HandleDeleteSG_BadJSON(t *testing.T) {
	_, nc := startTestNATS(t)
	topo, _ := sgTopoFixture(t)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	resp, err := nc.Request(TopicDeleteSG, []byte("not json"), 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "delete SG with malformed payload must fail")
}

func TestSG_HandleUpdateSG_BadJSON(t *testing.T) {
	_, nc := startTestNATS(t)
	topo, _ := sgTopoFixture(t)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	resp, err := nc.Request(TopicUpdateSG, []byte("not json"), 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "update SG with malformed payload must fail")
}
