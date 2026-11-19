package policy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
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
	sg := NewSecurityGroupManager(m, EgressPolicy{})

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
	// 6 infra + 2 ingress + 1 egress = 9 ACLs.
	assert.Len(t, storedPG.ACLs, 9)
}

func TestSGManager_EnsureSG_ReplacesACLSet(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m, EgressPolicy{})

	require.NoError(t, sg.EnsureSG(ctx, SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "10.0.0.0/8"}},
	}))
	firstCount := len(m.PortGroups[pg].ACLs)
	assert.Equal(t, 7, firstCount) // 6 infra + 1 tenant

	// Replace with a different set — wider ingress, no egress.
	require.NoError(t, sg.EnsureSG(ctx, SGSpec{
		GroupID: "sg-web",
		IngressRules: []Rule{
			{IPProtocol: "tcp", FromPort: 80, ToPort: 80, CIDR: "0.0.0.0/0"},
			{IPProtocol: "tcp", FromPort: 443, ToPort: 443, CIDR: "0.0.0.0/0"},
			{IPProtocol: "icmp", CIDR: "0.0.0.0/0"},
		},
	}))
	assert.Len(t, m.PortGroups[pg].ACLs, 9) // 6 infra + 3 tenant
}

func TestSGManager_EnsureSG_MissingPortGroup_ReturnsError(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	sg := NewSecurityGroupManager(m, EgressPolicy{})

	err := sg.EnsureSG(ctx, SGSpec{GroupID: "sg-missing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port group")
}

// TestSGManager_EnsureSG_AtomicReplace asserts repeated EnsureSG calls use
// ReplaceACLs (single OVSDB transaction) and never call ClearACLs.
func TestSGManager_EnsureSG_AtomicReplace(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m, EgressPolicy{})

	require.NoError(t, sg.EnsureSG(ctx, SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "10.0.0.0/8"}},
	}))
	require.NoError(t, sg.EnsureSG(ctx, SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 80, ToPort: 80, CIDR: "0.0.0.0/0"}},
	}))

	assert.Equal(t, 2, m.ReplaceACLCalls, "each EnsureSG must call ReplaceACLs")
	assert.Zero(t, m.ClearACLCalls, "EnsureSG must NOT call ClearACLs")
	assert.NotEmpty(t, m.PortGroups[pg].ACLs, "PG ACLs must never be empty after a replace")
}

func hasACLNamed(m *mock.Client, name string) bool {
	for _, a := range m.ACLs {
		if a.Name != nil && *a.Name == name {
			return true
		}
	}
	return false
}

func TestSGManager_EnsureSG_AppliesWANEgressBlock(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m, EgressPolicy{BlockedWANPorts: []int{25, 465, 587}})

	require.NoError(t, sg.EnsureSG(ctx, SGSpec{
		GroupID: "sg-web", VPCID: "vpc-1",
		EgressRules: []Rule{{IPProtocol: "-1", CIDR: "0.0.0.0/0"}},
	}))
	// 6 infra + 1 tenant egress + 1 WAN block = 8.
	assert.Len(t, m.PortGroups[pg].ACLs, 8)
	assert.True(t, hasACLNamed(m, pg+"-block-wan-egress"))
}

func TestSGManager_EnsureSG_NoBlockWhenPolicyEmpty(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m, EgressPolicy{})

	require.NoError(t, sg.EnsureSG(ctx, SGSpec{GroupID: "sg-web", VPCID: "vpc-1"}))
	assert.Len(t, m.PortGroups[pg].ACLs, 6) // infra only
	assert.False(t, hasACLNamed(m, pg+"-block-wan-egress"))
}

func TestSGManager_EnsureSG_ExemptVPCOmitsBlock(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m, EgressPolicy{
		BlockedWANPorts: []int{25},
		ExemptVPCs:      map[string]bool{"vpc-exempt": true},
	})

	require.NoError(t, sg.EnsureSG(ctx, SGSpec{GroupID: "sg-web", VPCID: "vpc-exempt"}))
	assert.Len(t, m.PortGroups[pg].ACLs, 6) // infra only, block skipped
	assert.False(t, hasACLNamed(m, pg+"-block-wan-egress"))
}

func TestSGManager_EnsureSG_Idempotent(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m, EgressPolicy{})

	spec := SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "10.0.0.0/8"}},
	}
	require.NoError(t, sg.EnsureSG(ctx, spec))
	first := len(m.PortGroups[pg].ACLs)
	require.NoError(t, sg.EnsureSG(ctx, spec))
	assert.Len(t, m.PortGroups[pg].ACLs, first)
}

// s3Lists knows one list, as vpcd's source does.
func s3Lists(id string, cidrs ...string) PrefixLists {
	return func(want string) ([]string, bool) {
		if want != id {
			return nil, false
		}
		return cidrs, true
	}
}

// The address set a rule names is written with the list's entries before the
// ACL naming it, in either direction, and converges when the entries change.
func TestSGManager_EnsureSG_WritesPrefixListAddressSet(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	entries := []string{"192.0.2.10/32", "192.0.2.11/32"}
	sg := NewSecurityGroupManager(m, EgressPolicy{}, WithPrefixLists(func(id string) ([]string, bool) {
		return entries, id == "pl-6ca54005"
	}))
	spec := SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 443, ToPort: 443, PrefixListID: "pl-6ca54005"}},
		EgressRules:  []Rule{{IPProtocol: "tcp", FromPort: 8443, ToPort: 8443, PrefixListID: "pl-6ca54005"}},
	}
	require.NoError(t, sg.EnsureSG(ctx, spec))
	require.Contains(t, m.AddressSets, "spx_pl_6ca54005")
	assert.ElementsMatch(t, entries, m.AddressSets["spx_pl_6ca54005"].Addresses)
	assert.Len(t, m.AddressSets, 1, "one set per list, however many rules name it")
	assert.Len(t, m.PortGroups[pg].ACLs, 8) // 6 infra + 2 tenant

	entries = []string{"192.0.2.12/32"}
	require.NoError(t, sg.EnsureSG(ctx, spec))
	assert.Equal(t, []string{"192.0.2.12/32"}, m.AddressSets["spx_pl_6ca54005"].Addresses)
}

// A list with no entries yields an empty set, which matches nothing.
func TestSGManager_EnsureSG_EmptyPrefixListIsAnEmptySet(t *testing.T) {
	ctx := context.Background()
	m, _ := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m, EgressPolicy{}, WithPrefixLists(s3Lists("pl-6ca54005")))
	require.NoError(t, sg.EnsureSG(ctx, SGSpec{
		GroupID:     "sg-web",
		EgressRules: []Rule{{IPProtocol: "-1", PrefixListID: "pl-6ca54005"}},
	}))
	require.Contains(t, m.AddressSets, "spx_pl_6ca54005")
	assert.NotNil(t, m.AddressSets["spx_pl_6ca54005"].Addresses)
	assert.Empty(t, m.AddressSets["spx_pl_6ca54005"].Addresses)
}

// A rule naming a list the source does not know fails the group, leaving its
// ACLs as they were, as an unrenderable protocol does.
func TestSGManager_EnsureSG_UnknownPrefixListFailsTheGroup(t *testing.T) {
	ctx := context.Background()
	for name, opts := range map[string][]SGOption{
		"no source":    nil,
		"unknown list": {WithPrefixLists(s3Lists("pl-6ca54005"))},
	} {
		t.Run(name, func(t *testing.T) {
			m, pg := newSGMockWithPG(t, "sg-web")
			sg := NewSecurityGroupManager(m, EgressPolicy{}, opts...)
			require.NoError(t, sg.EnsureSG(ctx, SGSpec{
				GroupID:     "sg-web",
				EgressRules: []Rule{{IPProtocol: "-1", CIDR: "0.0.0.0/0"}},
			}))
			before := len(m.PortGroups[pg].ACLs)
			err := sg.EnsureSG(ctx, SGSpec{
				GroupID: "sg-web",
				EgressRules: []Rule{
					{IPProtocol: "-1", CIDR: "0.0.0.0/0"},
					{IPProtocol: "tcp", FromPort: 443, ToPort: 443, PrefixListID: "pl-0123456789abcdef0"},
				},
			})
			require.ErrorContains(t, err, "unknown prefix list pl-0123456789abcdef0")
			assert.Len(t, m.PortGroups[pg].ACLs, before)
			assert.Empty(t, m.AddressSets)
		})
	}
}
