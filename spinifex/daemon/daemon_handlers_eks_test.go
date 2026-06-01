package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupEKSDaemon returns a Daemon with only its eksService field populated —
// every EKS handler dispatches through that field via handleNATSRequest, so
// the rest of the Daemon graph can stay nil.
func setupEKSDaemon(t *testing.T) (*Daemon, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := handlers_eks.NewEKSServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	return &Daemon{eksService: svc}, nc
}

// requestEKS publishes a request on the given subject and returns the response
// payload. Every EKS handler in this branch returns NotImplemented, so the
// payload always decodes to that error code.
func requestEKS(t *testing.T, nc *nats.Conn, subject string) []byte {
	t.Helper()
	msg := nats.NewMsg(subject)
	msg.Data = []byte(`{}`)
	msg.Header.Set(utils.AccountIDHeader, "111122223333")
	resp, err := nc.RequestMsg(msg, 2*time.Second)
	require.NoError(t, err, "no responder for %s", subject)
	return resp.Data
}

func assertNotImpl(t *testing.T, payload []byte) {
	t.Helper()
	var env struct {
		Code *string `json:"Code"`
	}
	require.NoError(t, json.Unmarshal(payload, &env), "decode payload %q", payload)
	require.NotNil(t, env.Code, "payload %q has no Code field", payload)
	assert.Equal(t, awserrors.ErrorNotImplemented, *env.Code)
}

func TestDaemonHandleEKS_AllHandlersDispatchToService(t *testing.T) {
	d, nc := setupEKSDaemon(t)

	cases := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{"eks.CreateCluster", d.handleEKSCreateCluster},
		{"eks.DescribeCluster", d.handleEKSDescribeCluster},
		{"eks.ListClusters", d.handleEKSListClusters},
		{"eks.UpdateClusterConfig", d.handleEKSUpdateClusterConfig},
		{"eks.UpdateClusterVersion", d.handleEKSUpdateClusterVersion},
		{"eks.DeleteCluster", d.handleEKSDeleteCluster},
		{"eks.CreateNodegroup", d.handleEKSCreateNodegroup},
		{"eks.DescribeNodegroup", d.handleEKSDescribeNodegroup},
		{"eks.ListNodegroups", d.handleEKSListNodegroups},
		{"eks.UpdateNodegroupConfig", d.handleEKSUpdateNodegroupConfig},
		{"eks.UpdateNodegroupVersion", d.handleEKSUpdateNodegroupVersion},
		{"eks.DeleteNodegroup", d.handleEKSDeleteNodegroup},
		{"eks.CreateAccessEntry", d.handleEKSCreateAccessEntry},
		{"eks.DescribeAccessEntry", d.handleEKSDescribeAccessEntry},
		{"eks.ListAccessEntries", d.handleEKSListAccessEntries},
		{"eks.UpdateAccessEntry", d.handleEKSUpdateAccessEntry},
		{"eks.DeleteAccessEntry", d.handleEKSDeleteAccessEntry},
		{"eks.AssociateAccessPolicy", d.handleEKSAssociateAccessPolicy},
		{"eks.DisassociateAccessPolicy", d.handleEKSDisassociateAccessPolicy},
		{"eks.ListAssociatedAccessPolicies", d.handleEKSListAssociatedAccessPolicies},
		{"eks.ListAccessPolicies", d.handleEKSListAccessPolicies},
		{"eks.ListAddons", d.handleEKSListAddons},
		{"eks.DescribeAddonVersions", d.handleEKSDescribeAddonVersions},
		{"eks.CreateAddon", d.handleEKSCreateAddon},
		{"eks.DeleteAddon", d.handleEKSDeleteAddon},
		{"eks.DescribeAddon", d.handleEKSDescribeAddon},
		{"eks.UpdateAddon", d.handleEKSUpdateAddon},
		{"eks.AssociateIdentityProviderConfig", d.handleEKSAssociateIdentityProviderConfig},
		{"eks.DescribeIdentityProviderConfig", d.handleEKSDescribeIdentityProviderConfig},
		{"eks.ListIdentityProviderConfigs", d.handleEKSListIdentityProviderConfigs},
		{"eks.DisassociateIdentityProviderConfig", d.handleEKSDisassociateIdentityProviderConfig},
		{"eks.TagResource", d.handleEKSTagResource},
		{"eks.UntagResource", d.handleEKSUntagResource},
		{"eks.ListTagsForResource", d.handleEKSListTagsForResource},
	}
	require.Equal(t, 34, len(cases), "expected exactly one handler per AWS EKS action")

	for _, c := range cases {
		t.Run(c.subject, func(t *testing.T) {
			sub, err := nc.Subscribe(c.subject, c.handler)
			require.NoError(t, err)
			t.Cleanup(func() { _ = sub.Unsubscribe() })

			payload := requestEKS(t, nc, c.subject)
			assertNotImpl(t, payload)
		})
	}
}
