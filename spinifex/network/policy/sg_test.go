package policy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

func newSGMockWithPG(t *testing.T, sgID string) (*mock.Client, string) {
	t.Helper()
	m := mock.New()
	pg := topology.SecurityGroupPortGroup(sgID)
	require.NoError(t, m.CreatePortGroup(context.Background(), pg, nil))
	return m, pg
}

func TestSGManager_EnsureSG_AppliesInfraPlusTenantACLs(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m)

	spec := SGSpec{
		GroupID: "sg-web",
		VPCID:   "vpc-1",
		IngressRules: []Rule{
			{IPProtocol: "tcp", FromPort: 80, ToPort: 80, CIDR: "0.0.0.0/0"},
			{IPProtocol: "tcp", FromPort: 443, ToPort: 443, CIDR: "0.0.0.0/0"},
		},
		EgressRules: []Rule{
			{IPProtocol: "-1", CIDR: "0.0.0.0/0"},
		},
	}
	require.NoError(t, sg.EnsureSG(ctx, spec))

	storedPG, ok := m.PortGroups[pg]
	require.True(t, ok)
	// 4 infra + 2 ingress + 1 egress = 7 ACLs.
	assert.Len(t, storedPG.ACLs, 7)
}

func TestSGManager_UpdateSG_ReplacesACLSet(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m)

	require.NoError(t, sg.EnsureSG(ctx, SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "10.0.0.0/8"}},
	}))
	firstCount := len(m.PortGroups[pg].ACLs)
	assert.Equal(t, 5, firstCount) // 4 infra + 1 tenant

	// Replace with a different set — wider ingress, no egress.
	require.NoError(t, sg.UpdateSG(ctx, SGSpec{
		GroupID: "sg-web",
		IngressRules: []Rule{
			{IPProtocol: "tcp", FromPort: 80, ToPort: 80, CIDR: "0.0.0.0/0"},
			{IPProtocol: "tcp", FromPort: 443, ToPort: 443, CIDR: "0.0.0.0/0"},
			{IPProtocol: "icmp", CIDR: "0.0.0.0/0"},
		},
	}))
	assert.Len(t, m.PortGroups[pg].ACLs, 7) // 4 infra + 3 tenant
}

func TestSGManager_DeleteSG_ClearsACLsKeepsPortGroup(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m)

	require.NoError(t, sg.EnsureSG(ctx, SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "-1", CIDR: "0.0.0.0/0"}},
	}))
	assert.NotEmpty(t, m.PortGroups[pg].ACLs)

	require.NoError(t, sg.DeleteSG(ctx, "sg-web"))
	assert.Empty(t, m.PortGroups[pg].ACLs)
	// Port group still present — L2 owns lifecycle.
	_, exists := m.PortGroups[pg]
	assert.True(t, exists)
}

func TestSGManager_EnsureSG_MissingPortGroup_ReturnsError(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	sg := NewSecurityGroupManager(m)

	err := sg.EnsureSG(ctx, SGSpec{GroupID: "sg-missing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port group")
}

// TestSGManager_UpdateSG_AtomicReplace asserts EnsureSG/UpdateSG use
// ReplaceACLs (single OVSDB transaction) and never call ClearACLs.
func TestSGManager_UpdateSG_AtomicReplace(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m)

	require.NoError(t, sg.EnsureSG(ctx, SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "10.0.0.0/8"}},
	}))
	require.NoError(t, sg.UpdateSG(ctx, SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 80, ToPort: 80, CIDR: "0.0.0.0/0"}},
	}))

	assert.Equal(t, 2, m.ReplaceACLCalls, "EnsureSG+UpdateSG must call ReplaceACLs")
	assert.Zero(t, m.ClearACLCalls, "EnsureSG/UpdateSG must NOT call ClearACLs")
	assert.NotEmpty(t, m.PortGroups[pg].ACLs, "PG ACLs must never be empty after UpdateSG")
}

func TestSGManager_EnsureSG_Idempotent(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m)

	spec := SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "10.0.0.0/8"}},
	}
	require.NoError(t, sg.EnsureSG(ctx, spec))
	first := len(m.PortGroups[pg].ACLs)
	require.NoError(t, sg.EnsureSG(ctx, spec))
	assert.Len(t, m.PortGroups[pg].ACLs, first)
}
