//test:in-package — drives EKS_Request through the gateway's unexported test
// helpers (withTestIdentity, policyMockIAMService) and auth context keys.

package gateway

import (
	"context"
	"errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/access"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchEKS drives the gateway with no NATS connection. A permitted request
// therefore reaches the NATS guard and fails there, which is what proves the
// policy check ran ahead of the resource existing at all.
func dispatchEKS(t *testing.T, gw *GatewayConfig, method, path, body string) error {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, "eks")
	ctx = context.WithValue(ctx, ctxAccountID, authzAccountID)
	req = withTestIdentity(req.WithContext(ctx))
	return gw.EKS_Request(httptest.NewRecorder(), req)
}

func eksARN(resource string) string {
	return "arn:aws:eks:" + authzRegion + ":" + authzAccountID + ":" + resource
}

// TestEKSRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production cluster; before the resolver the fence was inert and
// DeleteCluster against it was permitted with nothing logged.
func TestEKSRequest_ScopedDenyFires(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:DeleteCluster", "arn:aws:eks:*:*:cluster/prod"),
	)

	assertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod", ""))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev", ""))
}

// TestEKSRequest_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
func TestEKSRequest_ScopedAllowGrants(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:DeleteCluster", "arn:aws:eks:*:*:cluster/dev"),
	)

	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev", ""))
	assertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod", ""))
}

// The bypass this work closes. A tenant pastes the ARN out of DescribeNodegroup
// and fences it; before the discriminator was derived the gate evaluated a
// wildcarded spelling that this exact-ARN pattern could not match, so the fence
// was inert and the delete went through.
func TestEKSRequest_NodegroupScopeMatchesExactStoredARN(t *testing.T) {
	stored := arn.FormatEKSNodegroup(authzRegion, authzAccountID, "prod", "workers",
		arn.EKSNodegroupDiscriminator(authzAccountID, "prod", "workers"))

	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:DeleteNodegroup", stored),
	)

	assertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/workers", ""))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/batch", ""))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev/node-groups/workers", ""))
}

// The AWS-documented policy spelling wildcards the discriminator, and was the
// only spelling that worked before. It must keep working.
func TestEKSRequest_NodegroupScopeMatchesWildcardUUID(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:DeleteNodegroup", "arn:aws:eks:*:*:nodegroup/prod/workers/*"),
	)

	assertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/workers", ""))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/batch", ""))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev/node-groups/workers", ""))
}

// CreateCluster names its cluster in the body, so the body read has to happen
// before the gate for a fence on a name prefix to fire at all.
func TestEKSRequest_CreateClusterScopesFromBody(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:CreateCluster", "arn:aws:eks:*:*:cluster/prod-*"),
	)

	assertDenied(t, dispatchEKS(t, gw, http.MethodPost, "/clusters", `{"name":"prod-1"}`))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters", `{"name":"dev-1"}`))
	// An unreadable body authorizes account-wide and stays the handler's fault.
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters", "{not json"))
}

// The access-entry principal arrives percent-encoded in the path; the ARN the
// gate builds must be the one the handler's own builder produces.
func TestEKSRequest_AccessEntryPathIsUnescaped(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/app/admin"
	entry := arn.FormatEKSAccessEntry(authzRegion, authzAccountID, "prod", access.PrincipalARNHash(principal))

	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:DeleteAccessEntry", entry),
	)

	path := "/clusters/prod/access-entries/" + url.PathEscape(principal)
	assertDenied(t, dispatchEKS(t, gw, http.MethodDelete, path, ""))

	other := "/clusters/prod/access-entries/" + url.PathEscape("arn:aws:iam::123456789012:role/app/reader")
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, other, ""))
}

// A tag request names its resource by ARN. The handler ignores that ARN's
// account and works in the caller's, so the gate must too.
func TestEKSRequest_TagResourceScopesTheNamedResource(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:TagResource", eksARN("cluster/prod")),
	)

	assertDenied(t, dispatchEKS(t, gw, http.MethodPost,
		"/tags/"+url.PathEscape(eksARN("cluster/prod")), `{"tags":{"k":"v"}}`))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost,
		"/tags/"+url.PathEscape(eksARN("cluster/dev")), `{"tags":{"k":"v"}}`))
	// The same cluster spelled under another account is still the caller's.
	assertDenied(t, dispatchEKS(t, gw, http.MethodPost,
		"/tags/"+url.PathEscape("arn:aws:eks:us-east-1:999999999999:cluster/prod"), `{"tags":{"k":"v"}}`))
}

// The property the per-service rollout rests on: passing a real ARN where "*"
// was passed before cannot withdraw access a working policy already grants.
func TestEKSRequest_AccountWideGrantStillPermitsEveryAction(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "eks:*", "*"))
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/clusters", nil).
		WithContext(context.WithValue(context.Background(), ctxAccountID, authzAccountID)))

	for _, action := range gateway_eks.ScopedActions() {
		t.Run(action, func(t *testing.T) {
			resources, err := gateway_eks.ResourceARNs(action, authzRegion, authzAccountID,
				[]string{"prod", "workers", "extra"}, []byte(`{"name":"prod","nodegroupName":"workers","addonName":"coredns","principalArn":"arn:aws:iam::123456789012:role/app"}`))
			require.NoError(t, err)
			assert.NoError(t, gw.checkPolicyResources(req, "eks", action, resources))
		})
	}
}

// The internal CP-VM routes name the target account in the path, so eks:* on *
// evaluated as permitted and returned another tenant's cluster state. The
// principal gate now rejects the tenant ahead of the policy check.
func TestEKSRequest_InternalRoutesRejectTenantPrincipal(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "eks:*", "*"))

	assertDenied(t, dispatchEKS(t, gw, http.MethodGet, "/clusters/prod/internal-addons/999988887777", ""))
	assertDenied(t, dispatchEKS(t, gw, http.MethodGet,
		"/clusters/prod/internal-recovery/999988887777/i-0abc", ""))
	// Its own account is no different: a tenant is not a control-plane VM.
	assertDenied(t, dispatchEKS(t, gw, http.MethodGet, "/clusters/prod/internal-addons/"+authzAccountID, ""))
}

// dispatchEKSAsCPAgent drives the gateway with the auth context a CP VM's IMDS
// credentials actually produce, so the gate is exercised through eksCaller's
// field mapping rather than a hand-built Caller.
func dispatchEKSAsCPAgent(t *testing.T, gw *GatewayConfig, path, instanceID string) error {
	t.Helper()
	roleName := handlers_eks.CPInstanceRoleName
	req := httptest.NewRequest(http.MethodGet, path, nil)
	ctx := context.WithValue(req.Context(), ctxService, "eks")
	ctx = context.WithValue(ctx, ctxAccountID, awsidentifiers.GlobalAccountID)
	ctx = context.WithValue(ctx, ctxIdentity, instanceID)
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeAssumedRole)
	ctx = context.WithValue(ctx, ctxUnderlyingRoleARN,
		"arn:aws:iam::"+awsidentifiers.GlobalAccountID+":role/"+roleName)
	ctx = context.WithValue(ctx, ctxAssumedRoleARN,
		"arn:aws:sts::"+awsidentifiers.GlobalAccountID+":assumed-role/"+roleName+"/"+instanceID)
	return gw.EKS_Request(httptest.NewRecorder(), req.WithContext(ctx))
}

// The denial tests above all fail on the first clause of the class gate, so on
// their own they would still pass if eksCaller stopped producing a CP agent at
// all — which would deny every control-plane VM in production. This is the
// other half: the real IMDS-shaped context reaches the handler.
func TestEKSRequest_InternalRoutesAdmitTheCPAgentPrincipal(t *testing.T) {
	const cpInstanceID = "i-cp0000000000001"
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket: handlers_eks.AccountBucketName(authzAccountID),
	})
	require.NoError(t, err)
	require.NoError(t, handlers_eks.PutClusterMeta(t.Context(), kv, &handlers_eks.ClusterMeta{
		Name:              "prod",
		ControlPlaneNodes: []handlers_eks.ControlPlaneNode{{InstanceID: cpInstanceID}},
	}))

	gw := scopedPolicyGateway(statement("Allow", "eks:*", "*"))
	gw.NATSConn = nc

	// No eks daemon is subscribed, so the handler fails on the absent responder.
	// Anything but AccessDenied means the gate and the policy check both passed.
	assertNotDenied(t, dispatchEKSAsCPAgent(t, gw,
		"/clusters/prod/internal-addons/"+authzAccountID, cpInstanceID))
	assertNotDenied(t, dispatchEKSAsCPAgent(t, gw,
		"/clusters/prod/internal-recovery/"+authzAccountID+"/"+cpInstanceID, cpInstanceID))

	// Same credentials, a cluster this VM does not serve: the binding still bites.
	assertDenied(t, dispatchEKSAsCPAgent(t, gw,
		"/clusters/other/internal-addons/"+authzAccountID, cpInstanceID))
}

// The account-ID read moved above the gate, which used to reject a missing
// account itself. InternalError is the code the caller has always seen.
func TestEKSRequest_MissingAccountIDReturnsInternalError(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "eks:*", "*"))
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/clusters", nil).
		WithContext(context.WithValue(context.Background(), ctxService, "eks")))

	err := gw.EKS_Request(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

// publishFixture seeds two clusters in the caller's account and one in another,
// each served by its own control-plane VM, and subscribes to every EKS subject.
type publishFixture struct {
	gw  *GatewayConfig
	nc  *nats.Conn
	sub *nats.Subscription
}

const (
	publishCPAlpha    = "i-cp00000000000a1"
	publishCPBeta     = "i-cp00000000000b1"
	publishCPGamma    = "i-cp00000000000c1"
	publishOtherAcct  = "444455556666"
	publishNoClusters = "999988887777"
)

func newPublishFixture(t *testing.T) *publishFixture {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	for _, c := range []struct{ acct, cluster, member string }{
		{authzAccountID, "alpha", publishCPAlpha},
		{authzAccountID, "beta", publishCPBeta},
		{publishOtherAcct, "gamma", publishCPGamma},
	} {
		kv, err := handlers_eks.GetOrCreateAccountBucket(t.Context(), js, c.acct)
		require.NoError(t, err)
		require.NoError(t, handlers_eks.PutClusterMeta(t.Context(), kv, &handlers_eks.ClusterMeta{
			Name:              c.cluster,
			ControlPlaneNodes: []handlers_eks.ControlPlaneNode{{InstanceID: c.member}},
		}))
	}
	sub, err := nc.SubscribeSync("eks.>")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	gw := scopedPolicyGateway(statement("Allow", "eks:*", "*"))
	gw.NATSConn = nc
	return &publishFixture{gw: gw, nc: nc, sub: sub}
}

// publish POSTs body to cluster's internal-publish route under ctx's identity.
func (f *publishFixture) publish(t *testing.T, ctx context.Context, cluster, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/clusters/"+cluster+"/internal-publish", strings.NewReader(body))
	rec := httptest.NewRecorder()
	return rec, f.gw.EKS_Request(rec, req.WithContext(ctx))
}

// requireNothingPublished proves a denial stopped the relay, not just the reply.
func (f *publishFixture) requireNothingPublished(t *testing.T) {
	t.Helper()
	require.NoError(t, f.nc.Flush())
	msg, err := f.sub.NextMsg(100 * time.Millisecond)
	require.ErrorIs(t, err, nats.ErrTimeout, "relayed onto %v", msg)
}

// cpAgentContext is the auth context a CP VM's IMDS credentials produce.
func cpAgentContext(instanceID string) context.Context {
	roleName := handlers_eks.CPInstanceRoleName
	ctx := context.WithValue(context.Background(), ctxService, "eks")
	ctx = context.WithValue(ctx, ctxAccountID, awsidentifiers.GlobalAccountID)
	ctx = context.WithValue(ctx, ctxIdentity, instanceID)
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeAssumedRole)
	ctx = context.WithValue(ctx, ctxUnderlyingRoleARN,
		"arn:aws:iam::"+awsidentifiers.GlobalAccountID+":role/"+roleName)
	return context.WithValue(ctx, ctxAssumedRoleARN,
		"arn:aws:sts::"+awsidentifiers.GlobalAccountID+":assumed-role/"+roleName+"/"+instanceID)
}

func userContext(accountID, name string) context.Context {
	ctx := context.WithValue(context.Background(), ctxService, "eks")
	ctx = context.WithValue(ctx, ctxAccountID, accountID)
	ctx = context.WithValue(ctx, ctxIdentity, name)
	return context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
}

// publishChannel is one body shape PublishInternal relays, and where it lands.
type publishChannel struct {
	name    string
	fields  string
	subject func(accountID, cluster string) string
}

func publishChannels() []publishChannel {
	channels := []publishChannel{
		{"state", `"channel":"state"`, handlers_eks.StateSubject},
		{"addon", `"channel":"addon"`, eksv1.AddonStatusSubject},
	}
	for _, kind := range []string{
		handlers_eks.BootstrapSubjectToken, handlers_eks.BootstrapSubjectKubeconfig,
		handlers_eks.BootstrapSubjectJWKS, handlers_eks.BootstrapSubjectCA,
	} {
		channels = append(channels, publishChannel{
			name:   "bootstrap " + kind,
			fields: `"channel":"bootstrap","kind":"` + kind + `"`,
			subject: func(accountID, cluster string) string {
				return handlers_eks.BootstrapSubject(accountID, cluster, kind)
			},
		})
	}
	return channels
}

const publishPayload = `{"healthz":"ok", "node_count":1,"ts":42}`

func publishBody(accountID string, ch publishChannel) string {
	return `{"accountId":"` + accountID + `",` + ch.fields + `,"payload":` + publishPayload + `}`
}

// A CP VM reporting for the cluster it serves still reaches its subject, with
// the payload bytes relayed verbatim, on every channel.
func TestEKSRequest_PublishInternalRelaysForTheCallersOwnCluster(t *testing.T) {
	f := newPublishFixture(t)
	for _, ch := range publishChannels() {
		t.Run(ch.name, func(t *testing.T) {
			rec, err := f.publish(t, cpAgentContext(publishCPAlpha), "alpha", publishBody(authzAccountID, ch))
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.JSONEq(t, `{}`, rec.Body.String())

			msg, err := f.sub.NextMsg(2 * time.Second)
			require.NoError(t, err)
			assert.Equal(t, ch.subject(authzAccountID, "alpha"), msg.Subject)
			assert.Equal(t, publishPayload, string(msg.Data))
			f.requireNothingPublished(t)
		})
	}
}

// The bypass this closes: eks:PublishInternal on "*" let a CP VM report for any
// cluster in any account. Every target it does not serve is the same denial, so
// the reply cannot tell an existing cluster from an absent one.
func TestEKSRequest_PublishInternalDeniesAClusterTheCallerDoesNotServe(t *testing.T) {
	f := newPublishFixture(t)
	targets := []struct {
		name, account, cluster string
	}{
		{"its own account, another cluster", authzAccountID, "beta"},
		{"another account's cluster", publishOtherAcct, "gamma"},
		{"its own cluster's name in another account", publishOtherAcct, "alpha"},
		{"a cluster that does not exist", authzAccountID, "delta"},
		{"an account with no clusters", publishNoClusters, "alpha"},
		{"an account no bucket can be named for", publishOtherAcct + ".gamma", "alpha"},
		{"a cluster no key can be named for", authzAccountID, "*"},
	}
	for _, ch := range publishChannels() {
		for _, target := range targets {
			t.Run(ch.name+"/"+target.name, func(t *testing.T) {
				_, err := f.publish(t, cpAgentContext(publishCPAlpha), url.PathEscape(target.cluster), publishBody(target.account, ch))
				require.Error(t, err)
				assert.Equal(t, awserrors.ErrorAccessDenied, awserrors.ValidErrorCodeFromError(err))
				assert.Equal(t, errors.New(awserrors.ErrorAccessDenied), err)
				f.requireNothingPublished(t)
			})
		}
	}
}

// The account the gate binds is the one the subject is built from: a body that
// spells the account under another key, or adds a cluster field, moves nothing.
func TestEKSRequest_PublishInternalRedirectionIsDenied(t *testing.T) {
	f := newPublishFixture(t)
	state := publishChannels()[0]

	// URL names the caller's cluster, body names another account.
	_, err := f.publish(t, cpAgentContext(publishCPAlpha), "alpha", publishBody(publishOtherAcct, state))
	assertDenied(t, err)
	f.requireNothingPublished(t)

	// encoding/json folds case, so a re-cased key is the account the handler reads.
	_, err = f.publish(t, cpAgentContext(publishCPAlpha), "gamma",
		`{"AccountID":"`+publishOtherAcct+`","channel":"state","payload":{"x":1}}`)
	assertDenied(t, err)
	f.requireNothingPublished(t)

	// A repeated key decodes to its last value, which is what the gate binds.
	_, err = f.publish(t, cpAgentContext(publishCPAlpha), "alpha",
		`{"accountId":"`+authzAccountID+`","accountId":"`+publishOtherAcct+`","channel":"state","payload":{"x":1}}`)
	assertDenied(t, err)
	f.requireNothingPublished(t)

	// The body has no cluster field; the route's cluster is the only one used.
	_, err = f.publish(t, cpAgentContext(publishCPAlpha), "alpha",
		`{"accountId":"`+authzAccountID+`","clusterName":"beta","channel":"state","payload":{"x":1}}`)
	require.NoError(t, err)
	msg, err := f.sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, handlers_eks.StateSubject(authzAccountID, "alpha"), msg.Subject)
	f.requireNothingPublished(t)
}

// Only a CP VM's instance-role session may publish: a tenant holding eks:*, and
// baked static system credentials with no instance identity, are both refused.
func TestEKSRequest_PublishInternalRejectsNonCPCallers(t *testing.T) {
	f := newPublishFixture(t)
	callers := []struct {
		name string
		ctx  context.Context
	}{
		{"tenant user, own account", userContext(authzAccountID, "alice")},
		{"tenant user, another account", userContext(publishOtherAcct, "mallory")},
		{"static system credentials", userContext(awsidentifiers.GlobalAccountID, "admin")},
	}
	for _, ch := range publishChannels() {
		for _, caller := range callers {
			t.Run(ch.name+"/"+caller.name, func(t *testing.T) {
				_, err := f.publish(t, caller.ctx, "alpha", publishBody(authzAccountID, ch))
				assertDenied(t, err)
				f.requireNothingPublished(t)
			})
		}
	}
}
