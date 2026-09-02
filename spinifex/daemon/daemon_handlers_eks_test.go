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
	svc, err := handlers_eks.NewEKSServiceImpl(handlers_eks.EKSServiceDeps{NATSConn: nc})
	require.NoError(t, err)
	return &Daemon{eksService: svc}, nc
}

// requestEKS publishes a request on the given subject and returns the response
// payload. Proves a responder is wired for the subject and dispatched to the
// service. The expected Code per case is asserted by the caller; the empty `{}`
// body steers implemented handlers into a deterministic validation/dispatch
// error so the wiring is provable without a full orchestration graph.
func requestEKS(t *testing.T, nc *nats.Conn, subject string) []byte {
	t.Helper()
	msg := nats.NewMsg(subject)
	msg.Data = []byte(`{}`)
	msg.Header.Set(utils.AccountIDHeader, "111122223333")
	resp, err := nc.RequestMsg(msg, 2*time.Second)
	require.NoError(t, err, "no responder for %s", subject)
	return resp.Data
}

// assertErrorCode decodes the error envelope and asserts the AWS error Code.
func assertErrorCode(t *testing.T, payload []byte, want string) {
	t.Helper()
	var env struct {
		Code *string `json:"Code"`
	}
	require.NoError(t, json.Unmarshal(payload, &env), "decode payload %q", payload)
	require.NotNil(t, env.Code, "payload %q has no Code field", payload)
	assert.Equal(t, want, *env.Code)
}

func TestDaemonHandleEKS_AllHandlersDispatchToService(t *testing.T) {
	d, nc := setupEKSDaemon(t)

	// wantCode is the error Code an empty-body request must elicit, proving the
	// subject dispatched to its service method. Access-entry handlers are
	// implemented (Sprint 6c): an empty body fails input/cluster validation with
	// InvalidParameterValue. ListAccessPolicies returns the static catalogue with
	// no error, so wantCode is "" (success — assert a non-error payload).
	notImpl := awserrors.ErrorNotImplemented
	invalid := awserrors.ErrorInvalidParameterValue
	// Nodegroup mutators gate on orchestration deps, which the shim service
	// lacks → ServiceUnavailable. The read paths reach input validation and an
	// empty body fails with InvalidParameterValue. UpdateNodegroupVersion stays
	// NotImplemented (v1 doesn't do AMI upgrades).
	unavailable := awserrors.ErrorServiceUnavailable
	cases := []struct {
		subject  string
		handler  nats.MsgHandler
		wantCode string
	}{
		{"eks.UpdateClusterConfig", asMsgHandler(handleNATSRequest(d.node, d.eksService.UpdateClusterConfig)), notImpl},
		{"eks.UpdateClusterVersion", asMsgHandler(handleNATSRequest(d.node, d.eksService.UpdateClusterVersion)), notImpl},
		{"eks.CreateNodegroup", asMsgHandler(handleNATSRequest(d.node, d.eksService.CreateNodegroup)), unavailable},
		{"eks.DescribeNodegroup", asMsgHandler(handleNATSRequest(d.node, d.eksService.DescribeNodegroup)), invalid},
		{"eks.ListNodegroups", asMsgHandler(handleNATSRequest(d.node, d.eksService.ListNodegroups)), invalid},
		{"eks.UpdateNodegroupConfig", asMsgHandler(handleNATSRequest(d.node, d.eksService.UpdateNodegroupConfig)), unavailable},
		{"eks.UpdateNodegroupVersion", asMsgHandler(handleNATSRequest(d.node, d.eksService.UpdateNodegroupVersion)), notImpl},
		{"eks.DeleteNodegroup", asMsgHandler(handleNATSRequest(d.node, d.eksService.DeleteNodegroup)), unavailable},
		{"eks.CreateAccessEntry", asMsgHandler(handleNATSRequest(d.node, d.eksService.CreateAccessEntry)), invalid},
		{"eks.DescribeAccessEntry", asMsgHandler(handleNATSRequest(d.node, d.eksService.DescribeAccessEntry)), invalid},
		{"eks.ListAccessEntries", asMsgHandler(handleNATSRequest(d.node, d.eksService.ListAccessEntries)), invalid},
		{"eks.UpdateAccessEntry", asMsgHandler(handleNATSRequest(d.node, d.eksService.UpdateAccessEntry)), invalid},
		{"eks.DeleteAccessEntry", asMsgHandler(handleNATSRequest(d.node, d.eksService.DeleteAccessEntry)), invalid},
		{"eks.AssociateAccessPolicy", asMsgHandler(handleNATSRequest(d.node, d.eksService.AssociateAccessPolicy)), invalid},
		{"eks.DisassociateAccessPolicy", asMsgHandler(handleNATSRequest(d.node, d.eksService.DisassociateAccessPolicy)), invalid},
		{"eks.ListAssociatedAccessPolicies", asMsgHandler(handleNATSRequest(d.node, d.eksService.ListAssociatedAccessPolicies)), invalid},
		{"eks.ListAccessPolicies", asMsgHandler(handleNATSRequest(d.node, d.eksService.ListAccessPolicies)), ""},
		// Addon handlers are implemented (Sprint 6 P1): an empty body fails
		// cluster validation with InvalidParameterValue, except
		// DescribeAddonVersions which returns the static catalogue (success).
		{"eks.ListAddons", asMsgHandler(handleNATSRequest(d.node, d.eksService.ListAddons)), invalid},
		{"eks.DescribeAddonVersions", asMsgHandler(handleNATSRequest(d.node, d.eksService.DescribeAddonVersions)), ""},
		{"eks.CreateAddon", asMsgHandler(handleNATSRequest(d.node, d.eksService.CreateAddon)), invalid},
		{"eks.DeleteAddon", asMsgHandler(handleNATSRequest(d.node, d.eksService.DeleteAddon)), invalid},
		{"eks.DescribeAddon", asMsgHandler(handleNATSRequest(d.node, d.eksService.DescribeAddon)), invalid},
		{"eks.UpdateAddon", asMsgHandler(handleNATSRequest(d.node, d.eksService.UpdateAddon)), invalid},
		{"eks.AssociateIdentityProviderConfig", asMsgHandler(handleNATSRequest(d.node, d.eksService.AssociateIdentityProviderConfig)), notImpl},
		{"eks.DescribeIdentityProviderConfig", asMsgHandler(handleNATSRequest(d.node, d.eksService.DescribeIdentityProviderConfig)), notImpl},
		{"eks.ListIdentityProviderConfigs", asMsgHandler(handleNATSRequest(d.node, d.eksService.ListIdentityProviderConfigs)), notImpl},
		{"eks.DisassociateIdentityProviderConfig", asMsgHandler(handleNATSRequest(d.node, d.eksService.DisassociateIdentityProviderConfig)), notImpl},
		{"eks.TagResource", asMsgHandler(handleNATSRequest(d.node, d.eksService.TagResource)), notImpl},
		{"eks.UntagResource", asMsgHandler(handleNATSRequest(d.node, d.eksService.UntagResource)), notImpl},
		{"eks.ListTagsForResource", asMsgHandler(handleNATSRequest(d.node, d.eksService.ListTagsForResource)), notImpl},
	}
	require.Len(t, cases, 30, "expected exactly one handler per non-lifecycle AWS EKS action")

	for _, c := range cases {
		t.Run(c.subject, func(t *testing.T) {
			sub, err := nc.Subscribe(c.subject, c.handler)
			require.NoError(t, err)
			t.Cleanup(func() { _ = sub.Unsubscribe() })

			payload := requestEKS(t, nc, c.subject)
			if c.wantCode == "" {
				// Success payload: dispatch is proven by a decodable non-error body.
				var env struct {
					Code *string `json:"Code"`
				}
				require.NoError(t, json.Unmarshal(payload, &env), "decode payload %q", payload)
				assert.Nil(t, env.Code, "expected success, got error %q", payload)
				return
			}
			assertErrorCode(t, payload, c.wantCode)
		})
	}
}
