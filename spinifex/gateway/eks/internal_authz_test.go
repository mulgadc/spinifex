package gateway_eks_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The literal STS puts in the auth context, spelled out rather than shared
	// with the gate: a silent change to either side must fail a test, not agree
	// with itself.
	principalTypeAssumedRole = "assumed-role"

	tenantAccount = "111122223333"
	cpInstanceID  = "i-cp0000000000001"
	otherInstance = "i-cp0000000000002"
)

// cpAgent is the principal an IMDS-credentialed control-plane VM presents.
func cpAgent(instanceID string) gateway_eks.Caller {
	return gateway_eks.Caller{
		AccountID:     utils.GlobalAccountID,
		PrincipalType: principalTypeAssumedRole,
		RoleName:      handlers_eks.CPInstanceRoleName,
		SessionName:   instanceID,
	}
}

// seedCluster writes a cluster meta whose control plane is the given members.
func seedCluster(t *testing.T, nc *nats.Conn, accountID, cluster string, meta *handlers_eks.ClusterMeta) {
	t.Helper()
	js := testutil.NewJetStream(t, nc)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket: handlers_eks.AccountBucketName(accountID),
	})
	require.NoError(t, err)
	meta.Name = cluster
	require.NoError(t, handlers_eks.PutClusterMeta(t.Context(), kv, meta))
}

func assertDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

// A tenant holding eks:* names another account in the path. The class gate
// rejects it before any cluster state is read, so it holds with no NATS
// connection at all.
func TestAuthorizeInternal_RejectsNonCPPrincipals(t *testing.T) {
	cases := []struct {
		name   string
		caller gateway_eks.Caller
	}{
		{"tenant user with eks:*", gateway_eks.Caller{
			AccountID: tenantAccount, PrincipalType: "user", SessionName: "alice",
		}},
		{"tenant role session", gateway_eks.Caller{
			AccountID:     tenantAccount,
			PrincipalType: principalTypeAssumedRole,
			RoleName:      handlers_eks.CPInstanceRoleName,
			SessionName:   cpInstanceID,
		}},
		{"system user, not a role session", gateway_eks.Caller{
			AccountID: utils.GlobalAccountID, PrincipalType: "user", SessionName: "root",
		}},
		{"another role in the system account", gateway_eks.Caller{
			AccountID:     utils.GlobalAccountID,
			PrincipalType: principalTypeAssumedRole,
			RoleName:      "spinifex-rds-instance",
			SessionName:   cpInstanceID,
		}},
		{"no session name", gateway_eks.Caller{
			AccountID:     utils.GlobalAccountID,
			PrincipalType: principalTypeAssumedRole,
			RoleName:      handlers_eks.CPInstanceRoleName,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDenied(t, gateway_eks.AuthorizeInternal(t.Context(), nil, "ListInternalAddons",
				tc.caller, []string{"alpha", tenantAccount}))
			assertDenied(t, gateway_eks.AuthorizeInternal(t.Context(), nil, "GetRecoveryDirective",
				tc.caller, []string{"alpha", tenantAccount, cpInstanceID}))
		})
	}
}

// Actions outside the internal set keep their own gating; this one adds none.
func TestAuthorizeInternal_IgnoresOtherActions(t *testing.T) {
	assert.False(t, gateway_eks.IsInternalAction("DescribeCluster"))
	require.NoError(t, gateway_eks.AuthorizeInternal(t.Context(), nil, "DescribeCluster",
		gateway_eks.Caller{AccountID: tenantAccount, PrincipalType: "user"}, []string{"alpha"}))
}

func TestAuthorizeInternal_AllowsClusterMember(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	seedCluster(t, nc, tenantAccount, "alpha", &handlers_eks.ClusterMeta{
		ControlPlaneNodes: []handlers_eks.ControlPlaneNode{
			{InstanceID: otherInstance}, {InstanceID: cpInstanceID},
		},
	})

	require.NoError(t, gateway_eks.AuthorizeInternal(t.Context(), nc, "ListInternalAddons",
		cpAgent(cpInstanceID), []string{"alpha", tenantAccount}))
	require.NoError(t, gateway_eks.AuthorizeInternal(t.Context(), nc, "GetRecoveryDirective",
		cpAgent(cpInstanceID), []string{"alpha", tenantAccount, cpInstanceID}))
}

// Clusters persisted before ControlPlaneNodes existed carry the member in the
// scalar field only, and their VMs must keep reaching the routes.
func TestAuthorizeInternal_AllowsLegacyScalarMember(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	seedCluster(t, nc, tenantAccount, "alpha", &handlers_eks.ClusterMeta{
		ControlPlaneInstanceID: cpInstanceID,
	})

	require.NoError(t, gateway_eks.AuthorizeInternal(t.Context(), nc, "ListInternalAddons",
		cpAgent(cpInstanceID), []string{"alpha", tenantAccount}))
}

// A CP VM that is a genuine agent still cannot name an account or a cluster it
// does not serve, which is what the path segment previously let it do.
func TestAuthorizeInternal_RejectsClusterTheCallerDoesNotServe(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	seedCluster(t, nc, tenantAccount, "alpha", &handlers_eks.ClusterMeta{
		ControlPlaneNodes: []handlers_eks.ControlPlaneNode{{InstanceID: cpInstanceID}},
	})
	seedCluster(t, nc, "444455556666", "beta", &handlers_eks.ClusterMeta{
		ControlPlaneNodes: []handlers_eks.ControlPlaneNode{{InstanceID: otherInstance}},
	})

	cases := []struct {
		name    string
		account string
		cluster string
	}{
		{"another account's cluster", "444455556666", "beta"},
		{"an account with no clusters", "999988887777", "alpha"},
		{"a cluster that does not exist", tenantAccount, "gamma"},
		{"its own account, another cluster", tenantAccount, "beta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDenied(t, gateway_eks.AuthorizeInternal(t.Context(), nc, "ListInternalAddons",
				cpAgent(cpInstanceID), []string{tc.cluster, tc.account}))
		})
	}
}

// A member reads its own directive and no other member's.
func TestAuthorizeInternal_RecoveryIsSelfOnly(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	seedCluster(t, nc, tenantAccount, "alpha", &handlers_eks.ClusterMeta{
		ControlPlaneNodes: []handlers_eks.ControlPlaneNode{
			{InstanceID: cpInstanceID}, {InstanceID: otherInstance},
		},
	})

	assertDenied(t, gateway_eks.AuthorizeInternal(t.Context(), nc, "GetRecoveryDirective",
		cpAgent(cpInstanceID), []string{"alpha", tenantAccount, otherInstance}))
}

func TestAuthorizeInternal_RejectsEmptyPathSegments(t *testing.T) {
	for _, params := range [][]string{{"", tenantAccount}, {"alpha", ""}, {"alpha"}} {
		err := gateway_eks.AuthorizeInternal(t.Context(), nil, "ListInternalAddons", cpAgent(cpInstanceID), params)
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	}
}

// A CP agent whose lookup cannot run at all is an infrastructure fault, not a
// denial: reporting AccessDenied would send the on-VM agent's retry budget
// chasing a permission it already has.
func TestAuthorizeInternal_NoNATSIsServerInternal(t *testing.T) {
	err := gateway_eks.AuthorizeInternal(t.Context(), nil, "ListInternalAddons",
		cpAgent(cpInstanceID), []string{"alpha", tenantAccount})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
