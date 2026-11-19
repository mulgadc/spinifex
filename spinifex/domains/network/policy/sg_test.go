package policy

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
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

// A security-group apply creates an absent set empty before the ACL naming it
// and never rewrites an existing set's members; only the reconcile pass fills
// and converges them.
func TestSGManager_EnsureSG_CreatesPrefixListSetEmpty(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	entries := []string{"192.0.2.10/32", "192.0.2.11/32"}
	sg := NewSecurityGroupManager(m, EgressPolicy{}, WithPrefixLists(func(id string) ([]string, bool) {
		return entries, id == "pl-6ca54005"
	}, "pl-6ca54005"))
	spec := SGSpec{
		GroupID:      "sg-web",
		IngressRules: []Rule{{IPProtocol: "tcp", FromPort: 443, ToPort: 443, PrefixListID: "pl-6ca54005"}},
		EgressRules:  []Rule{{IPProtocol: "tcp", FromPort: 8443, ToPort: 8443, PrefixListID: "pl-6ca54005"}},
	}
	require.NoError(t, sg.EnsureSG(ctx, spec))
	require.Contains(t, m.AddressSets, "spx_pl_6ca54005")
	assert.Empty(t, m.AddressSets["spx_pl_6ca54005"].Addresses, "a request grants nothing until the pass fills the set")
	assert.Len(t, m.AddressSets, 1, "one set per list, however many rules name it")
	assert.Len(t, m.PortGroups[pg].ACLs, 8) // 6 infra + 2 tenant

	require.NoError(t, sg.ConvergePrefixLists(ctx))
	assert.ElementsMatch(t, entries, m.AddressSets["spx_pl_6ca54005"].Addresses)

	entries = []string{"192.0.2.12/32"}
	require.NoError(t, sg.EnsureSG(ctx, spec))
	assert.Len(t, m.AddressSets["spx_pl_6ca54005"].Addresses, 2, "an apply leaves an existing set's members alone")
	require.NoError(t, sg.ConvergePrefixLists(ctx))
	assert.Equal(t, []string{"192.0.2.12/32"}, m.AddressSets["spx_pl_6ca54005"].Addresses)
}

// A list with no entries converges to an empty set, which matches nothing.
func TestSGManager_ConvergePrefixLists_EmptyListIsAnEmptySet(t *testing.T) {
	ctx := context.Background()
	m, _ := newSGMockWithPG(t, "sg-web")
	sg := NewSecurityGroupManager(m, EgressPolicy{}, WithPrefixLists(s3Lists("pl-6ca54005"), "pl-6ca54005"))
	require.NoError(t, sg.ConvergePrefixLists(ctx))
	require.Contains(t, m.AddressSets, "spx_pl_6ca54005")
	assert.NotNil(t, m.AddressSets["spx_pl_6ca54005"].Addresses)
	assert.Empty(t, m.AddressSets["spx_pl_6ca54005"].Addresses)
}

// While prefix-list grants are withdrawn, an apply renders no prefix-list ACL
// and writes no set, the pass converges nothing, and only applies that started
// while grants held are counted.
func TestSGManager_WithdrawnGrantsRenderNoPrefixListPeer(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	var granted atomic.Bool
	blocking := &blockingACLs{Client: m}
	sg := NewSecurityGroupManager(blocking, EgressPolicy{},
		WithPrefixLists(s3Lists("pl-6ca54005", "192.0.2.10/32"), "pl-6ca54005"),
		WithPrefixListGrants(granted.Load))
	spec := SGSpec{
		GroupID: "sg-web",
		EgressRules: []Rule{
			{IPProtocol: "-1", CIDR: "10.0.0.0/16"},
			{IPProtocol: "tcp", FromPort: 443, ToPort: 443, PrefixListID: "pl-6ca54005"},
		},
	}

	require.NoError(t, sg.EnsureSG(ctx, spec))
	for _, uuid := range m.PortGroups[pg].ACLs {
		assert.NotContains(t, m.ACLs[uuid].Match, "spx_pl_", "no ACL names a prefix-list set while grants are withdrawn")
	}
	assert.Len(t, m.PortGroups[pg].ACLs, 7) // 6 infra + the CIDR rule
	require.NoError(t, sg.ConvergePrefixLists(ctx))
	assert.Empty(t, m.AddressSets)
	assert.Zero(t, sg.GrantingApplies())

	granted.Store(true)
	blocking.entered, blocking.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- sg.EnsureSG(ctx, spec) }()
	<-blocking.entered
	granted.Store(false)
	assert.Equal(t, 1, sg.GrantingApplies(), "an apply that started while grants held is counted until it returns")
	close(blocking.release)
	require.NoError(t, <-done)
	assert.Zero(t, sg.GrantingApplies())
}

// An apply paused while it reads the grant is already counted, so a rollback
// that reports no apply in flight is one every later grant read observes.
func TestSGManager_ApplyIsCountedBeforeItReadsTheGrant(t *testing.T) {
	ctx := context.Background()
	m, pg := newSGMockWithPG(t, "sg-web")
	reading, answer := make(chan struct{}), make(chan bool)
	sg := NewSecurityGroupManager(m, EgressPolicy{},
		WithPrefixLists(s3Lists("pl-6ca54005", "192.0.2.10/32"), "pl-6ca54005"),
		WithPrefixListGrants(func() bool { reading <- struct{}{}; return <-answer }))
	spec := SGSpec{GroupID: "sg-web", EgressRules: []Rule{
		{IPProtocol: "tcp", FromPort: 443, ToPort: 443, PrefixListID: "pl-6ca54005"},
	}}

	done := make(chan error, 1)
	go func() { done <- sg.EnsureSG(ctx, spec) }()
	<-reading
	assert.Equal(t, 1, sg.GrantingApplies(), "an apply is counted before it reads the grant")
	answer <- false
	require.NoError(t, <-done)
	assert.Zero(t, sg.GrantingApplies())
	for _, uuid := range m.PortGroups[pg].ACLs {
		assert.NotContains(t, m.ACLs[uuid].Match, "spx_pl_", "an apply that read a withdrawn grant writes no prefix-list ACL")
	}
}

// blockingACLs holds ReplaceACLs open while release is set.
type blockingACLs struct {
	*mock.Client

	entered, release chan struct{}
}

func (b *blockingACLs) ReplaceACLs(ctx context.Context, pg string, specs []ovn.ACLSpec) error {
	if b.release != nil {
		b.entered <- struct{}{}
		<-b.release
	}
	return b.Client.ReplaceACLs(ctx, pg, specs)
}

// A rule naming a list the source does not know fails the group, leaving its
// ACLs as they were, as an unrenderable protocol does.
func TestSGManager_EnsureSG_UnknownPrefixListFailsTheGroup(t *testing.T) {
	ctx := context.Background()
	for name, opts := range map[string][]SGOption{
		"no source":    nil,
		"unknown list": {WithPrefixLists(s3Lists("pl-6ca54005"), "pl-6ca54005")},
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
