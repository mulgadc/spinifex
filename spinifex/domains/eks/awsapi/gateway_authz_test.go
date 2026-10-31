package awsapi_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/access"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eksGateway is the policy harness plus the NATS connection the registration is
// composed with; a nil connection fails the actions that reach the daemon.
type eksGateway struct {
	*gwtest.Harness

	nc    *nats.Conn
	roles passrole.Resolver
}

func scopedEKS(statements ...iamdomain.Statement) *eksGateway {
	return &eksGateway{Harness: gwtest.Scoped(statements...)}
}

// as returns the same gateway acting as caller, in caller's own account.
func (g *eksGateway) as(caller credential.Principal) *eksGateway {
	h := *g.Harness
	h.AccountID = caller.AccountID
	h.Caller = &caller
	return &eksGateway{Harness: &h, nc: g.nc}
}

func (g *eksGateway) serve(w http.ResponseWriter, r *http.Request) error {
	return g.Dispatch(eksawsapi.NewRegistration(eksawsapi.Deps{NATS: g.nc, Roles: g.roles}), eksawsapi.ServiceName, w, r)
}

func assertNotDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.NotEqual(t, awserrors.ErrorAccessDenied, code)
}

// dispatchEKS drives the gateway with no NATS connection. A permitted request
// therefore reaches the NATS guard and fails there, which is what proves the
// policy check ran ahead of the resource existing at all.
func dispatchEKS(t *testing.T, gw *eksGateway, method, path, body string) error {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	return gw.serve(httptest.NewRecorder(), req)
}

func eksARN(resource string) string {
	return "arn:aws:eks:" + gwtest.Region + ":" + gwtest.AccountID + ":" + resource
}

// TestEKSRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production cluster; before the resolver the fence was inert and
// DeleteCluster against it was permitted with nothing logged.
func TestEKSRequest_ScopedDenyFires(t *testing.T) {
	gw := scopedEKS(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Deny", "eks:DeleteCluster", "arn:aws:eks:*:*:cluster/prod"),
	)

	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod", ""))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev", ""))
}

// TestEKSRequest_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
func TestEKSRequest_ScopedAllowGrants(t *testing.T) {
	gw := scopedEKS(
		gwtest.Statement("Allow", "eks:DeleteCluster", "arn:aws:eks:*:*:cluster/dev"),
	)

	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev", ""))
	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod", ""))
}

// The bypass this work closes. A tenant pastes the ARN out of DescribeNodegroup
// and fences it; before the discriminator was derived the gate evaluated a
// wildcarded spelling that this exact-ARN pattern could not match, so the fence
// was inert and the delete went through.
func TestEKSRequest_NodegroupScopeMatchesExactStoredARN(t *testing.T) {
	stored := arn.FormatEKSNodegroup(gwtest.Region, gwtest.AccountID, "prod", "workers",
		arn.EKSNodegroupDiscriminator(gwtest.AccountID, "prod", "workers"))

	gw := scopedEKS(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Deny", "eks:DeleteNodegroup", stored),
	)

	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/workers", ""))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/batch", ""))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev/node-groups/workers", ""))
}

// The AWS-documented policy spelling wildcards the discriminator, and was the
// only spelling that worked before. It must keep working.
func TestEKSRequest_NodegroupScopeMatchesWildcardUUID(t *testing.T) {
	gw := scopedEKS(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Deny", "eks:DeleteNodegroup", "arn:aws:eks:*:*:nodegroup/prod/workers/*"),
	)

	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/workers", ""))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/batch", ""))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev/node-groups/workers", ""))
}

// CreateCluster names its cluster in the body, so the body read has to happen
// before the gate for a fence on a name prefix to fire at all.
func TestEKSRequest_CreateClusterScopesFromBody(t *testing.T) {
	gw := scopedEKS(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Deny", "eks:CreateCluster", "arn:aws:eks:*:*:cluster/prod-*"),
	)

	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodPost, "/clusters", `{"name":"prod-1"}`))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters", `{"name":"dev-1"}`))
	// An unreadable body authorizes account-wide and stays the handler's fault.
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters", "{not json"))
}

// The access-entry principal arrives percent-encoded in the path; the ARN the
// gate builds must be the one the handler's own builder produces.
func TestEKSRequest_AccessEntryPathIsUnescaped(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/app/admin"
	entry := arn.FormatEKSAccessEntry(gwtest.Region, gwtest.AccountID, "prod", access.PrincipalARNHash(principal))

	gw := scopedEKS(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Deny", "eks:DeleteAccessEntry", entry),
	)

	path := "/clusters/prod/access-entries/" + url.PathEscape(principal)
	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodDelete, path, ""))

	other := "/clusters/prod/access-entries/" + url.PathEscape("arn:aws:iam::123456789012:role/app/reader")
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, other, ""))
}

// A tag request names its resource by ARN. The handler ignores that ARN's
// account and works in the caller's, so the gate must too.
func TestEKSRequest_TagResourceScopesTheNamedResource(t *testing.T) {
	gw := scopedEKS(
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.Statement("Deny", "eks:TagResource", eksARN("cluster/prod")),
	)

	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodPost,
		"/tags/"+url.PathEscape(eksARN("cluster/prod")), `{"tags":{"k":"v"}}`))
	gwtest.AssertPermitted(t, dispatchEKS(t, gw, http.MethodPost,
		"/tags/"+url.PathEscape(eksARN("cluster/dev")), `{"tags":{"k":"v"}}`))
	// The same cluster spelled under another account is still the caller's.
	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodPost,
		"/tags/"+url.PathEscape("arn:aws:eks:us-east-1:999999999999:cluster/prod"), `{"tags":{"k":"v"}}`))
}

// The property the per-service rollout rests on: passing a real ARN where "*"
// was passed before cannot withdraw access a working policy already grants.
func TestEKSRequest_AccountWideGrantStillPermitsEveryAction(t *testing.T) {
	gw := scopedEKS(gwtest.Statement("Allow", "eks:*", "*"))
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	caller := userContext(gwtest.AccountID, "alice")

	for _, action := range eksawsapi.ScopedActions() {
		t.Run(action, func(t *testing.T) {
			resources, err := eksawsapi.ResourceARNs(action, gwtest.Region, gwtest.AccountID,
				[]string{"prod", "workers", "extra"}, []byte(`{"name":"prod","nodegroupName":"workers","addonName":"coredns","principalArn":"arn:aws:iam::123456789012:role/app"}`))
			require.NoError(t, err)
			assert.NoError(t, pipeline.Authorize(gw.Policy, req, caller, true, "eks", action, resources, nil))
		})
	}
}

// The internal CP-VM routes name the target account in the path, so eks:* on *
// evaluated as permitted and returned another tenant's cluster state. The
// principal gate now rejects the tenant ahead of the policy check.
func TestEKSRequest_InternalRoutesRejectTenantPrincipal(t *testing.T) {
	gw := scopedEKS(gwtest.Statement("Allow", "eks:*", "*"))

	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodGet, "/clusters/prod/internal-addons/999988887777", ""))
	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodGet,
		"/clusters/prod/internal-recovery/999988887777/i-0abc", ""))
	// Its own account is no different: a tenant is not a control-plane VM.
	gwtest.AssertDenied(t, dispatchEKS(t, gw, http.MethodGet, "/clusters/prod/internal-addons/"+gwtest.AccountID, ""))
}

// dispatchEKSAsCPAgent drives the gateway with the auth context a CP VM's IMDS
// credentials actually produce, so the gate is exercised through eksCaller's
// field mapping rather than a hand-built Caller.
func dispatchEKSAsCPAgent(t *testing.T, gw *eksGateway, path, instanceID string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	return gw.as(cpAgentContext(instanceID)).serve(httptest.NewRecorder(), req)
}

// The denial tests above all fail on the first clause of the class gate, so on
// their own they would still pass if eksCaller stopped producing a CP agent at
// all — which would deny every control-plane VM in production. This is the
// other half: the real IMDS-shaped context reaches the handler.
func TestEKSRequest_InternalRoutesAdmitTheCPAgentPrincipal(t *testing.T) {
	const cpInstanceID = "i-cp0000000000001"
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket: ekscluster.AccountBucketName(gwtest.AccountID),
	})
	require.NoError(t, err)
	require.NoError(t, ekscluster.PutClusterMeta(t.Context(), kv, &ekscluster.ClusterMeta{
		Name:              "prod",
		ControlPlaneNodes: []ekscluster.ControlPlaneNode{{InstanceID: cpInstanceID}},
	}))

	gw := scopedEKS(gwtest.Statement("Allow", "eks:*", "*"))
	gw.nc = nc

	// No eks daemon is subscribed, so the handler fails on the absent responder.
	// Anything but AccessDenied means the gate and the policy check both passed.
	assertNotDenied(t, dispatchEKSAsCPAgent(t, gw,
		"/clusters/prod/internal-addons/"+gwtest.AccountID, cpInstanceID))
	assertNotDenied(t, dispatchEKSAsCPAgent(t, gw,
		"/clusters/prod/internal-recovery/"+gwtest.AccountID+"/"+cpInstanceID, cpInstanceID))

	// Same credentials, a cluster this VM does not serve: the binding still bites.
	gwtest.AssertDenied(t, dispatchEKSAsCPAgent(t, gw,
		"/clusters/other/internal-addons/"+gwtest.AccountID, cpInstanceID))
}

// The account-ID read moved above the gate, which used to reject a missing
// account itself. InternalError is the code the caller has always seen.
func TestEKSRequest_MissingAccountIDReturnsInternalError(t *testing.T) {
	gw := scopedEKS(gwtest.Statement("Allow", "eks:*", "*"))
	gw.AccountID = ""
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)

	err := gw.serve(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

// publishFixture seeds two clusters in the caller's account and one in another,
// each served by its own control-plane VM, and subscribes to every EKS subject.
type publishFixture struct {
	gw  *eksGateway
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
		{gwtest.AccountID, "alpha", publishCPAlpha},
		{gwtest.AccountID, "beta", publishCPBeta},
		{publishOtherAcct, "gamma", publishCPGamma},
	} {
		kv, err := ekscluster.GetOrCreateAccountBucket(t.Context(), js, c.acct)
		require.NoError(t, err)
		require.NoError(t, ekscluster.PutClusterMeta(t.Context(), kv, &ekscluster.ClusterMeta{
			Name:              c.cluster,
			ControlPlaneNodes: []ekscluster.ControlPlaneNode{{InstanceID: c.member}},
		}))
	}
	sub, err := nc.SubscribeSync("eks.>")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	gw := scopedEKS(gwtest.Statement("Allow", "eks:*", "*"))
	gw.nc = nc
	return &publishFixture{gw: gw, nc: nc, sub: sub}
}

// publish POSTs body to cluster's internal-publish route under ctx's identity.
func (f *publishFixture) publish(t *testing.T, caller credential.Principal, cluster, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/clusters/"+cluster+"/internal-publish", strings.NewReader(body))
	rec := httptest.NewRecorder()
	return rec, f.gw.as(caller).serve(rec, req)
}

// requireNothingPublished proves a denial stopped the relay, not just the reply.
func (f *publishFixture) requireNothingPublished(t *testing.T) {
	t.Helper()
	require.NoError(t, f.nc.Flush())
	pending, _, err := f.sub.Pending()
	require.NoError(t, err)
	require.Zero(t, pending, "relayed onto eks.>")
}

// cpAgentContext is the caller a CP VM's IMDS credentials produce.
func cpAgentContext(instanceID string) credential.Principal {
	roleName := ekscluster.CPInstanceRoleName
	assumed := "arn:aws:sts::" + awsidentifiers.GlobalAccountID + ":assumed-role/" + roleName + "/" + instanceID
	return credential.Principal{
		AccountID:         awsidentifiers.GlobalAccountID,
		Identity:          instanceID,
		PrincipalType:     dispatch.PrincipalTypeAssumedRole,
		UnderlyingRoleARN: "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":role/" + roleName,
		AssumedRoleARN:    assumed,
		CallerARN:         assumed,
	}
}

func userContext(accountID, name string) credential.Principal {
	return credential.Principal{
		AccountID:     accountID,
		Identity:      name,
		PrincipalType: stsdomain.PrincipalTypeUser,
		CallerARN:     "arn:aws:iam::" + accountID + ":user/" + name,
	}
}

// publishChannel is one body shape PublishInternal relays, and where it lands.
type publishChannel struct {
	name    string
	fields  string
	subject func(accountID, cluster string) string
}

func publishChannels() []publishChannel {
	channels := []publishChannel{
		{"state", `"channel":"state"`, ekscluster.StateSubject},
		{"addon", `"channel":"addon"`, eksv1.AddonStatusSubject},
	}
	for _, kind := range []string{
		ekscluster.BootstrapSubjectToken, ekscluster.BootstrapSubjectKubeconfig,
		ekscluster.BootstrapSubjectJWKS, ekscluster.BootstrapSubjectCA,
	} {
		channels = append(channels, publishChannel{
			name:   "bootstrap " + kind,
			fields: `"channel":"bootstrap","kind":"` + kind + `"`,
			subject: func(accountID, cluster string) string {
				return ekscluster.BootstrapSubject(accountID, cluster, kind)
			},
		})
	}
	return channels
}

const publishPayload = `{"healthz":"ok", "node_count":1,"ts":42}`

func relayBody(accountID string, ch publishChannel) string {
	return `{"accountId":"` + accountID + `",` + ch.fields + `,"payload":` + publishPayload + `}`
}

// A CP VM reporting for the cluster it serves still reaches its subject, with
// the payload bytes relayed verbatim, on every channel.
func TestEKSRequest_PublishInternalRelaysForTheCallersOwnCluster(t *testing.T) {
	f := newPublishFixture(t)
	for _, ch := range publishChannels() {
		t.Run(ch.name, func(t *testing.T) {
			rec, err := f.publish(t, cpAgentContext(publishCPAlpha), "alpha", relayBody(gwtest.AccountID, ch))
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.JSONEq(t, `{}`, rec.Body.String())

			msg, err := f.sub.NextMsg(2 * time.Second)
			require.NoError(t, err)
			assert.Equal(t, ch.subject(gwtest.AccountID, "alpha"), msg.Subject)
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
		{"its own account, another cluster", gwtest.AccountID, "beta"},
		{"another account's cluster", publishOtherAcct, "gamma"},
		{"its own cluster's name in another account", publishOtherAcct, "alpha"},
		{"a cluster that does not exist", gwtest.AccountID, "delta"},
		{"an account with no clusters", publishNoClusters, "alpha"},
		{"an account no bucket can be named for", publishOtherAcct + ".gamma", "alpha"},
		{"a cluster no key can be named for", gwtest.AccountID, "*"},
	}
	for _, ch := range publishChannels() {
		for _, target := range targets {
			t.Run(ch.name+"/"+target.name, func(t *testing.T) {
				_, err := f.publish(t, cpAgentContext(publishCPAlpha), url.PathEscape(target.cluster), relayBody(target.account, ch))
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
	_, err := f.publish(t, cpAgentContext(publishCPAlpha), "alpha", relayBody(publishOtherAcct, state))
	gwtest.AssertDenied(t, err)
	f.requireNothingPublished(t)

	// encoding/json folds case, so a re-cased key is the account the handler reads.
	_, err = f.publish(t, cpAgentContext(publishCPAlpha), "gamma",
		`{"AccountID":"`+publishOtherAcct+`","channel":"state","payload":{"x":1}}`)
	gwtest.AssertDenied(t, err)
	f.requireNothingPublished(t)

	// A repeated key decodes to its last value, which is what the gate binds.
	_, err = f.publish(t, cpAgentContext(publishCPAlpha), "alpha",
		`{"accountId":"`+gwtest.AccountID+`","accountId":"`+publishOtherAcct+`","channel":"state","payload":{"x":1}}`)
	gwtest.AssertDenied(t, err)
	f.requireNothingPublished(t)

	// The body has no cluster field; the route's cluster is the only one used.
	_, err = f.publish(t, cpAgentContext(publishCPAlpha), "alpha",
		`{"accountId":"`+gwtest.AccountID+`","clusterName":"beta","channel":"state","payload":{"x":1}}`)
	require.NoError(t, err)
	msg, err := f.sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, ekscluster.StateSubject(gwtest.AccountID, "alpha"), msg.Subject)
	f.requireNothingPublished(t)
}

// Only a CP VM's instance-role session may publish: a tenant holding eks:*, and
// baked static system credentials with no instance identity, are both refused.
func TestEKSRequest_PublishInternalRejectsNonCPCallers(t *testing.T) {
	f := newPublishFixture(t)
	callers := []struct {
		name string
		ctx  credential.Principal
	}{
		{"tenant user, own account", userContext(gwtest.AccountID, "alice")},
		{"tenant user, another account", userContext(publishOtherAcct, "mallory")},
		{"static system credentials", userContext(awsidentifiers.GlobalAccountID, "admin")},
	}
	for _, ch := range publishChannels() {
		for _, caller := range callers {
			t.Run(ch.name+"/"+caller.name, func(t *testing.T) {
				_, err := f.publish(t, caller.ctx, "alpha", relayBody(gwtest.AccountID, ch))
				gwtest.AssertDenied(t, err)
				f.requireNothingPublished(t)
			})
		}
	}
}

// tokenReviewFixture is publishFixture's clusters with an access entry on each,
// granting one principal a group named for its cluster, and a stand-in STS
// verifier that records which cluster each review was pinned to.
type tokenReviewFixture struct {
	gw       *eksGateway
	verified chan string
}

const tokenReviewPrincipal = "arn:aws:iam::" + gwtest.AccountID + ":role/admin"

var tokenReviewToken = "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte("https://sts/?Action=GetCallerIdentity"))

func newTokenReviewFixture(t *testing.T) *tokenReviewFixture {
	t.Helper()
	f := newPublishFixture(t)
	js, err := jetstream.New(f.nc)
	require.NoError(t, err)
	for _, c := range []struct{ acct, cluster string }{
		{gwtest.AccountID, "alpha"}, {gwtest.AccountID, "beta"}, {publishOtherAcct, "gamma"},
	} {
		kv, err := ekscluster.GetOrCreateAccountBucket(t.Context(), js, c.acct)
		require.NoError(t, err)
		_, err = access.New(gwtest.Region).Create(t.Context(), kv, c.acct, access.Spec{
			Cluster: c.cluster, PrincipalARN: tokenReviewPrincipal,
			Groups: []string{"group-" + c.cluster}, Type: access.EntryTypeStandard,
		})
		require.NoError(t, err)
	}

	verified := make(chan string, 16)
	sub, err := f.nc.Subscribe(ekscluster.TokenVerifySubject, func(m *nats.Msg) {
		var req ekscluster.TokenVerifyRequest
		_ = json.Unmarshal(m.Data, &req)
		verified <- req.ClusterName
		resp, _ := json.Marshal(ekscluster.TokenVerifyResponse{
			AccountID: gwtest.AccountID, ARN: tokenReviewPrincipal, UserID: "AROAEXAMPLE:session",
		})
		_ = m.Respond(resp)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &tokenReviewFixture{gw: f.gw, verified: verified}
}

func reviewBody(accountID string) string {
	return `{"accountId":"` + accountID + `","token":"` + tokenReviewToken + `"}`
}

func (f *tokenReviewFixture) review(t *testing.T, caller credential.Principal, cluster, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/clusters/"+cluster+"/token-review", strings.NewReader(body))
	rec := httptest.NewRecorder()
	return rec, f.gw.as(caller).serve(rec, req)
}

func (f *tokenReviewFixture) nextVerified(t *testing.T) string {
	t.Helper()
	select {
	case cluster := <-f.verified:
		return cluster
	case <-time.After(2 * time.Second):
		t.Fatal("token was never verified")
		return ""
	}
}

// requireNothingVerified proves a denial stopped the review before STS, not
// just the reply.
func (f *tokenReviewFixture) requireNothingVerified(t *testing.T) {
	t.Helper()
	select {
	case cluster := <-f.verified:
		t.Fatalf("token verified against cluster %q", cluster)
	default:
	}
}

// A denial that differs by target would tell the caller which clusters exist.
func requireIndistinguishableDenial(t *testing.T, rec *httptest.ResponseRecorder, err error) {
	t.Helper()
	assert.Equal(t, errors.New(awserrors.ErrorAccessDenied), err)
	assert.Equal(t, awserrors.ErrorAccessDenied, awserrors.ValidErrorCodeFromError(err))
	assert.Empty(t, rec.Body.String())
}

// The CP VM's webhook reviewing a token for the cluster it serves gets the same
// identity it always did, verified against that cluster.
func TestEKSRequest_WebhookTokenReviewResolvesForTheCallersOwnCluster(t *testing.T) {
	f := newTokenReviewFixture(t)
	rec, err := f.review(t, cpAgentContext(publishCPAlpha), "alpha", reviewBody(gwtest.AccountID))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"authenticated":true,"username":"`+tokenReviewPrincipal+`",`+
		`"uid":"AROAEXAMPLE:session","groups":["group-alpha"]}`, rec.Body.String())
	assert.Equal(t, "alpha", f.nextVerified(t))
	f.requireNothingVerified(t)
}

// The bypass this closes: eks:WebhookTokenReview on "*" let a CP VM resolve a
// token against any cluster in any account, reading the identities and groups
// mapped there. Every target it does not serve is the same denial.
func TestEKSRequest_WebhookTokenReviewDeniesAClusterTheCallerDoesNotServe(t *testing.T) {
	f := newTokenReviewFixture(t)
	targets := []struct {
		name, account, cluster string
	}{
		{"its own account, another cluster", gwtest.AccountID, "beta"},
		{"another account's cluster", publishOtherAcct, "gamma"},
		{"its own cluster's name in another account", publishOtherAcct, "alpha"},
		{"a cluster that does not exist", gwtest.AccountID, "delta"},
		{"an account with no clusters", publishNoClusters, "alpha"},
		{"an account no bucket can be named for", publishOtherAcct + ".gamma", "alpha"},
		{"a cluster no key can be named for", gwtest.AccountID, "*"},
	}
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			rec, err := f.review(t, cpAgentContext(publishCPAlpha), url.PathEscape(target.cluster), reviewBody(target.account))
			requireIndistinguishableDenial(t, rec, err)
			f.requireNothingVerified(t)
		})
	}
}

// The account the gate binds is the one the review reads access entries from,
// and the route's cluster is the only cluster: the body cannot name another.
func TestEKSRequest_WebhookTokenReviewRedirectionIsDenied(t *testing.T) {
	f := newTokenReviewFixture(t)
	cases := []struct{ name, cluster, body string }{
		{"URL names its cluster, body another account", "alpha", reviewBody(publishOtherAcct)},
		{"re-cased key the handler folds onto accountId", "gamma",
			`{"AccountID":"` + publishOtherAcct + `","token":"` + tokenReviewToken + `"}`},
		{"repeated key, whose last value the handler reads", "gamma",
			`{"accountId":"` + gwtest.AccountID + `","accountId":"` + publishOtherAcct + `","token":"` + tokenReviewToken + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := f.review(t, cpAgentContext(publishCPAlpha), tc.cluster, tc.body)
			requireIndistinguishableDenial(t, rec, err)
			f.requireNothingVerified(t)
		})
	}

	rec, err := f.review(t, cpAgentContext(publishCPAlpha), "alpha",
		`{"accountId":"`+gwtest.AccountID+`","clusterName":"beta","token":"`+tokenReviewToken+`"}`)
	require.NoError(t, err)
	assert.Contains(t, rec.Body.String(), `"group-alpha"`)
	assert.Equal(t, "alpha", f.nextVerified(t))
}

// Only a CP VM's instance-role session may review: a tenant holding eks:*, and
// baked static system credentials with no instance identity, are both refused.
func TestEKSRequest_WebhookTokenReviewRejectsNonCPCallers(t *testing.T) {
	f := newTokenReviewFixture(t)
	cases := []struct {
		name, cluster, account string
		ctx                    credential.Principal
	}{
		{"tenant, its own cluster", "alpha", gwtest.AccountID, userContext(gwtest.AccountID, "alice")},
		{"tenant, another account's cluster", "gamma", publishOtherAcct, userContext(gwtest.AccountID, "alice")},
		{"other account's tenant, this cluster", "alpha", gwtest.AccountID, userContext(publishOtherAcct, "mallory")},
		{"static system credentials", "alpha", gwtest.AccountID, userContext(awsidentifiers.GlobalAccountID, "admin")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := f.review(t, tc.ctx, tc.cluster, reviewBody(tc.account))
			requireIndistinguishableDenial(t, rec, err)
			f.requireNothingVerified(t)
		})
	}
}

// A body the handler cannot read stays the CP agent's validation fault; a
// tenant sending one is refused on its class before the body matters.
func TestEKSRequest_WebhookTokenReviewMalformedBody(t *testing.T) {
	f := newTokenReviewFixture(t)
	for _, body := range []string{`{not-json`, `{"token":"` + tokenReviewToken + `"}`, `{"accountId":"` + gwtest.AccountID + `","token":""}`} {
		_, err := f.review(t, cpAgentContext(publishCPAlpha), "alpha", body)
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err), "body %q", body)

		rec, err := f.review(t, userContext(gwtest.AccountID, "alice"), "alpha", body)
		requireIndistinguishableDenial(t, rec, err)
	}
	f.requireNothingVerified(t)
}
