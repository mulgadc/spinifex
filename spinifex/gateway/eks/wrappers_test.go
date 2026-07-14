package gateway_eks

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEKSResponder subscribes a catch-all on `eks.>` that replies with an
// empty JSON object for every request. Lets the gateway wrappers run their
// full NATSRequest round-trip without standing up a real EKSServiceImpl.
func stubEKSResponder(t *testing.T, nc *nats.Conn) {
	t.Helper()
	sub, err := nc.Subscribe("eks.>", func(m *nats.Msg) {
		if m.Reply == "" {
			return
		}
		_ = m.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

const acct = "111122223333"

func TestGatewayWrappers_Cluster(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := CreateCluster(context.Background(), nc, acct, "arn:aws:iam::111122223333:role/dev", []byte(`{"name":"alpha"}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := CreateCluster(context.Background(), nc, acct, "", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := DescribeCluster(context.Background(), nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := ListClusters(context.Background(), nc, acct)
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := UpdateClusterConfig(context.Background(), nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := UpdateClusterConfig(context.Background(), nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := UpdateClusterVersion(context.Background(), nc, acct, "alpha", []byte(`{"version":"1.30"}`))
	require.NoError(t, err)
	assert.NotNil(t, out7)

	out8, err := UpdateClusterVersion(context.Background(), nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out8)

	out9, err := DeleteCluster(context.Background(), nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out9)
}

func TestGatewayWrappers_Cluster_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := CreateCluster(context.Background(), nc, acct, "", []byte(`{not-json`))
	require.Error(t, err)
	_, err = UpdateClusterConfig(context.Background(), nc, acct, "alpha", []byte(`{not-json`))
	require.Error(t, err)
	_, err = UpdateClusterVersion(context.Background(), nc, acct, "alpha", []byte(`{not-json`))
	require.Error(t, err)
}

func TestGatewayWrappers_Nodegroup(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := CreateNodegroup(context.Background(), nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := CreateNodegroup(context.Background(), nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := DescribeNodegroup(context.Background(), nc, acct, "alpha", "ng1")
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := ListNodegroups(context.Background(), nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := UpdateNodegroupConfig(context.Background(), nc, acct, "alpha", "ng1", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := UpdateNodegroupConfig(context.Background(), nc, acct, "alpha", "ng1", nil)
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := UpdateNodegroupVersion(context.Background(), nc, acct, "alpha", "ng1", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out7)

	out8, err := UpdateNodegroupVersion(context.Background(), nc, acct, "alpha", "ng1", nil)
	require.NoError(t, err)
	assert.NotNil(t, out8)

	out9, err := DeleteNodegroup(context.Background(), nc, acct, "alpha", "ng1")
	require.NoError(t, err)
	assert.NotNil(t, out9)
}

func TestGatewayWrappers_Nodegroup_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := CreateNodegroup(context.Background(), nc, acct, "alpha", []byte(`{nope`))
	require.Error(t, err)
	_, err = UpdateNodegroupConfig(context.Background(), nc, acct, "alpha", "ng1", []byte(`{nope`))
	require.Error(t, err)
	_, err = UpdateNodegroupVersion(context.Background(), nc, acct, "alpha", "ng1", []byte(`{nope`))
	require.Error(t, err)
}

func TestGatewayWrappers_Access(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := CreateAccessEntry(context.Background(), nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := CreateAccessEntry(context.Background(), nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := DescribeAccessEntry(context.Background(), nc, acct, "alpha", "arn-user")
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := ListAccessEntries(context.Background(), nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := UpdateAccessEntry(context.Background(), nc, acct, "alpha", "arn-user", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := UpdateAccessEntry(context.Background(), nc, acct, "alpha", "arn-user", nil)
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := DeleteAccessEntry(context.Background(), nc, acct, "alpha", "arn-user")
	require.NoError(t, err)
	assert.NotNil(t, out7)

	out8, err := AssociateAccessPolicy(context.Background(), nc, acct, "alpha", "arn-user", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out8)

	out9, err := AssociateAccessPolicy(context.Background(), nc, acct, "alpha", "arn-user", nil)
	require.NoError(t, err)
	assert.NotNil(t, out9)

	out10, err := DisassociateAccessPolicy(context.Background(), nc, acct, "alpha", "arn-user", "policy-arn")
	require.NoError(t, err)
	assert.NotNil(t, out10)

	out12, err := ListAssociatedAccessPolicies(context.Background(), nc, acct, "alpha", "arn-user")
	require.NoError(t, err)
	assert.NotNil(t, out12)

	out13, err := ListAccessPolicies(context.Background(), nc, acct)
	require.NoError(t, err)
	assert.NotNil(t, out13)
}

func TestGatewayWrappers_Access_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := CreateAccessEntry(context.Background(), nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
	_, err = UpdateAccessEntry(context.Background(), nc, acct, "alpha", "arn-user", []byte(`{x`))
	require.Error(t, err)
	_, err = AssociateAccessPolicy(context.Background(), nc, acct, "alpha", "arn-user", []byte(`{x`))
	require.Error(t, err)
}

func TestGatewayWrappers_Addons(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := ListAddons(context.Background(), nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := DescribeAddonVersions(context.Background(), nc, acct)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := CreateAddon(context.Background(), nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := CreateAddon(context.Background(), nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := DescribeAddon(context.Background(), nc, acct, "alpha", "vpc-cni")
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := DeleteAddon(context.Background(), nc, acct, "alpha", "vpc-cni")
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := UpdateAddon(context.Background(), nc, acct, "alpha", "vpc-cni", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out7)

	out8, err := UpdateAddon(context.Background(), nc, acct, "alpha", "vpc-cni", nil)
	require.NoError(t, err)
	assert.NotNil(t, out8)
}

func TestGatewayWrappers_Addons_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := CreateAddon(context.Background(), nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
	_, err = UpdateAddon(context.Background(), nc, acct, "alpha", "vpc-cni", []byte(`{x`))
	require.Error(t, err)
}

func TestGatewayWrappers_OIDC(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := AssociateIdentityProviderConfig(context.Background(), nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := AssociateIdentityProviderConfig(context.Background(), nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := DescribeIdentityProviderConfig(context.Background(), nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := DescribeIdentityProviderConfig(context.Background(), nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := ListIdentityProviderConfigs(context.Background(), nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := DisassociateIdentityProviderConfig(context.Background(), nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := DisassociateIdentityProviderConfig(context.Background(), nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out7)
}

func TestGatewayWrappers_OIDC_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := AssociateIdentityProviderConfig(context.Background(), nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
	_, err = DescribeIdentityProviderConfig(context.Background(), nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
	_, err = DisassociateIdentityProviderConfig(context.Background(), nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
}

func TestGatewayWrappers_Tags(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := TagResource(context.Background(), nc, acct, "arn-cluster", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := TagResource(context.Background(), nc, acct, "arn-cluster", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := UntagResource(context.Background(), nc, acct, "arn-cluster", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := UntagResource(context.Background(), nc, acct, "arn-cluster", nil)
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := ListTagsForResource(context.Background(), nc, acct, "arn-cluster")
	require.NoError(t, err)
	assert.NotNil(t, out5)
}

func TestGatewayWrappers_Tags_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := TagResource(context.Background(), nc, acct, "arn-cluster", []byte(`{x`))
	require.Error(t, err)
	_, err = UntagResource(context.Background(), nc, acct, "arn-cluster", []byte(`{x`))
	require.Error(t, err)
}
