package gateway_eks

import (
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

	out1, err := CreateCluster(nc, acct, "arn:aws:iam::111122223333:role/dev", []byte(`{"name":"alpha"}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := CreateCluster(nc, acct, "", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := DescribeCluster(nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := ListClusters(nc, acct)
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := UpdateClusterConfig(nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := UpdateClusterConfig(nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := UpdateClusterVersion(nc, acct, "alpha", []byte(`{"version":"1.30"}`))
	require.NoError(t, err)
	assert.NotNil(t, out7)

	out8, err := UpdateClusterVersion(nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out8)

	out9, err := DeleteCluster(nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out9)
}

func TestGatewayWrappers_Cluster_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := CreateCluster(nc, acct, "", []byte(`{not-json`))
	require.Error(t, err)
	_, err = UpdateClusterConfig(nc, acct, "alpha", []byte(`{not-json`))
	require.Error(t, err)
	_, err = UpdateClusterVersion(nc, acct, "alpha", []byte(`{not-json`))
	require.Error(t, err)
}

func TestGatewayWrappers_Nodegroup(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := CreateNodegroup(nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := CreateNodegroup(nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := DescribeNodegroup(nc, acct, "alpha", "ng1")
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := ListNodegroups(nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := UpdateNodegroupConfig(nc, acct, "alpha", "ng1", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := UpdateNodegroupConfig(nc, acct, "alpha", "ng1", nil)
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := UpdateNodegroupVersion(nc, acct, "alpha", "ng1", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out7)

	out8, err := UpdateNodegroupVersion(nc, acct, "alpha", "ng1", nil)
	require.NoError(t, err)
	assert.NotNil(t, out8)

	out9, err := DeleteNodegroup(nc, acct, "alpha", "ng1")
	require.NoError(t, err)
	assert.NotNil(t, out9)
}

func TestGatewayWrappers_Nodegroup_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := CreateNodegroup(nc, acct, "alpha", []byte(`{nope`))
	require.Error(t, err)
	_, err = UpdateNodegroupConfig(nc, acct, "alpha", "ng1", []byte(`{nope`))
	require.Error(t, err)
	_, err = UpdateNodegroupVersion(nc, acct, "alpha", "ng1", []byte(`{nope`))
	require.Error(t, err)
}

func TestGatewayWrappers_Access(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := CreateAccessEntry(nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := CreateAccessEntry(nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := DescribeAccessEntry(nc, acct, "alpha", "arn-user")
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := ListAccessEntries(nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := UpdateAccessEntry(nc, acct, "alpha", "arn-user", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := UpdateAccessEntry(nc, acct, "alpha", "arn-user", nil)
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := DeleteAccessEntry(nc, acct, "alpha", "arn-user")
	require.NoError(t, err)
	assert.NotNil(t, out7)

	out8, err := AssociateAccessPolicy(nc, acct, "alpha", "arn-user", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out8)

	out9, err := AssociateAccessPolicy(nc, acct, "alpha", "arn-user", nil)
	require.NoError(t, err)
	assert.NotNil(t, out9)

	out10, err := DisassociateAccessPolicy(nc, acct, "alpha", "arn-user", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out10)

	out11, err := DisassociateAccessPolicy(nc, acct, "alpha", "arn-user", nil)
	require.NoError(t, err)
	assert.NotNil(t, out11)

	out12, err := ListAssociatedAccessPolicies(nc, acct, "alpha", "arn-user")
	require.NoError(t, err)
	assert.NotNil(t, out12)

	out13, err := ListAccessPolicies(nc, acct)
	require.NoError(t, err)
	assert.NotNil(t, out13)
}

func TestGatewayWrappers_Access_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := CreateAccessEntry(nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
	_, err = UpdateAccessEntry(nc, acct, "alpha", "arn-user", []byte(`{x`))
	require.Error(t, err)
	_, err = AssociateAccessPolicy(nc, acct, "alpha", "arn-user", []byte(`{x`))
	require.Error(t, err)
	_, err = DisassociateAccessPolicy(nc, acct, "alpha", "arn-user", []byte(`{x`))
	require.Error(t, err)
}

func TestGatewayWrappers_Addons(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := ListAddons(nc, acct)
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := DescribeAddonVersions(nc, acct)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := CreateAddon(nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := CreateAddon(nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := DescribeAddon(nc, acct, "alpha", "vpc-cni")
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := DeleteAddon(nc, acct, "alpha", "vpc-cni")
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := UpdateAddon(nc, acct, "alpha", "vpc-cni", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out7)

	out8, err := UpdateAddon(nc, acct, "alpha", "vpc-cni", nil)
	require.NoError(t, err)
	assert.NotNil(t, out8)
}

func TestGatewayWrappers_Addons_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := CreateAddon(nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
	_, err = UpdateAddon(nc, acct, "alpha", "vpc-cni", []byte(`{x`))
	require.Error(t, err)
}

func TestGatewayWrappers_OIDC(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := AssociateIdentityProviderConfig(nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := AssociateIdentityProviderConfig(nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := DescribeIdentityProviderConfig(nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := DescribeIdentityProviderConfig(nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := ListIdentityProviderConfigs(nc, acct, "alpha")
	require.NoError(t, err)
	assert.NotNil(t, out5)

	out6, err := DisassociateIdentityProviderConfig(nc, acct, "alpha", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out6)

	out7, err := DisassociateIdentityProviderConfig(nc, acct, "alpha", nil)
	require.NoError(t, err)
	assert.NotNil(t, out7)
}

func TestGatewayWrappers_OIDC_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := AssociateIdentityProviderConfig(nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
	_, err = DescribeIdentityProviderConfig(nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
	_, err = DisassociateIdentityProviderConfig(nc, acct, "alpha", []byte(`{x`))
	require.Error(t, err)
}

func TestGatewayWrappers_Tags(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	out1, err := TagResource(nc, acct, "arn-cluster", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out1)

	out2, err := TagResource(nc, acct, "arn-cluster", nil)
	require.NoError(t, err)
	assert.NotNil(t, out2)

	out3, err := UntagResource(nc, acct, "arn-cluster", []byte(`{}`))
	require.NoError(t, err)
	assert.NotNil(t, out3)

	out4, err := UntagResource(nc, acct, "arn-cluster", nil)
	require.NoError(t, err)
	assert.NotNil(t, out4)

	out5, err := ListTagsForResource(nc, acct, "arn-cluster")
	require.NoError(t, err)
	assert.NotNil(t, out5)
}

func TestGatewayWrappers_Tags_BadJSON(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubEKSResponder(t, nc)

	_, err := TagResource(nc, acct, "arn-cluster", []byte(`{x`))
	require.Error(t, err)
	_, err = UntagResource(nc, acct, "arn-cluster", []byte(`{x`))
	require.Error(t, err)
}
