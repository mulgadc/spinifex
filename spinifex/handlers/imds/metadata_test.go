package handlers_imds

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----- fakes -------------------------------------------------------------

type fakeResolver struct {
	eni        *eniFacts
	eniErr     error
	inst       *instanceFacts
	instErr    error
	sgNames    map[string]string // sg-id → group name; absent IDs fall back to the ID
	subnetCIDR string
	subnetErr  error
	vpcCIDR    string
	vpcErr     error
}

func (f *fakeResolver) resolveENIByID(_ string) (*eniFacts, error) { return f.eni, f.eniErr }
func (f *fakeResolver) resolveInstance(_ *eniFacts) (*instanceFacts, error) {
	return f.inst, f.instErr
}

func (f *fakeResolver) resolveSGNames(_ string, sgIDs []string) []string {
	out := make([]string, len(sgIDs))
	for i, id := range sgIDs {
		if name, ok := f.sgNames[id]; ok {
			out[i] = name
		} else {
			out[i] = id
		}
	}
	return out
}

func (f *fakeResolver) resolveSubnetCIDR(_, _ string) (string, error) {
	return f.subnetCIDR, f.subnetErr
}
func (f *fakeResolver) resolveVPCCIDR(_, _ string) (string, error) { return f.vpcCIDR, f.vpcErr }

type fakeIAM struct {
	profile    *handlers_iam.InstanceProfile
	profileErr error
	roleARN    string
	roleErr    error
}

func (f *fakeIAM) ResolveInstanceProfile(_, _ string) (*handlers_iam.InstanceProfile, error) {
	return f.profile, f.profileErr
}

func (f *fakeIAM) GetRole(_ string, _ *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	if f.roleErr != nil {
		return nil, f.roleErr
	}
	return &iam.GetRoleOutput{Role: &iam.Role{Arn: aws.String(f.roleARN)}}, nil
}

// fakePublicKeys is the publicKeyLookup seam for the public-keys path tests. It
// counts invocations so a test can assert the directory listings never reach the
// material RPC.
type fakePublicKeys struct {
	material string
	err      error
	calls    int
}

func (f *fakePublicKeys) GetPublicKey(_, _ string) (string, error) {
	f.calls++
	return f.material, f.err
}

const (
	testVPC = "vpc-abc12345"
	testIP  = "10.0.1.5"
)

func testENI() *eniFacts {
	return &eniFacts{
		eniID:            "eni-aaa",
		accountID:        "111122223333",
		instanceID:       "i-0123456789",
		vpcID:            testVPC,
		subnetID:         "subnet-1",
		privateIP:        testIP,
		publicIP:         "203.0.113.7",
		mac:              "02:11:22:33:44:55",
		availabilityZone: "ap-southeast-2a",
		securityGroupIDs: []string{"sg-1", "sg-2"},
	}
}

// withTapENI wraps a handler to thread the per-tap ENI identity into every
// request, the way the per-tap responder's BaseContext does in production. A nil
// eni leaves the context empty so resolveCaller misses and the request 404s.
func withTapENI(h http.Handler, eni *eniFacts) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if eni != nil {
			r = r.WithContext(context.WithValue(r.Context(), ctxKeyENI, eni))
		}
		h.ServeHTTP(w, r)
	})
}

// newTestService builds an IMDSServiceImpl with injected fakes and a fixed
// clock. The per-tap responder is left nil — the HTTP handler is exercised directly.
func newTestService(res eniResolver, fIAM profileLookup, assumer stsAssumer) (*IMDSServiceImpl, time.Time) {
	now := time.Unix(1_700_000_000, 0).UTC()
	return &IMDSServiceImpl{
		resolver: res,
		tokens:   newTokenStore(),
		creds:    newCredCache(assumer),
		iam:      fIAM,
		pubKeys:  &fakePublicKeys{},
		now:      func() time.Time { return now },
	}, now
}

// newPubKeyTestService builds a service whose publicKeyLookup is the supplied
// fake, so the public-keys tests can drive the material RPC and assert call counts.
func newPubKeyTestService(res eniResolver, pk publicKeyLookup) *IMDSServiceImpl {
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	svc.pubKeys = pk
	return svc
}

// get issues a token-gated GET, plus the supplied token header. The tap ENI
// identity is threaded by the withTapENI-wrapped handler.
func get(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+MetaDataServerIP+path, nil)
	req.RemoteAddr = testIP + ":50000"
	if token != "" {
		req.Header.Set(hdrToken, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// issueToken performs the PUT /latest/api/token handshake and returns the token.
func issueToken(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "http://"+MetaDataServerIP+pathToken, nil)
	req.RemoteAddr = testIP + ":50000"
	req.Header.Set(hdrTokenTTL, "60")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, rec.Body.String())
	return rec.Body.String()
}

// ----- token model ------------------------------------------------------

func TestHTTP_TokenlessGETRejected(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	rec := get(t, h, prefixMetaData+"instance-id", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestHTTP_TokenPUTValidation(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	for _, ttl := range []string{"", "0", "21601", "notanumber"} {
		req := httptest.NewRequest(http.MethodPut, "http://"+MetaDataServerIP+pathToken, nil)
		req.RemoteAddr = testIP + ":50000"
		if ttl != "" {
			req.Header.Set(hdrTokenTTL, ttl)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "ttl=%q", ttl)
	}
}

func TestHTTP_TokenGETMethodNotAllowed(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	req := httptest.NewRequest(http.MethodGet, "http://"+MetaDataServerIP+pathToken, nil)
	req.RemoteAddr = testIP + ":50000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestHTTP_ForwardedForRejected pins AWS IMDS's SSRF defence: any request
// carrying X-Forwarded-For is refused with 403 regardless of token or identity,
// on both the read path and token issuance.
func TestHTTP_ForwardedForRejected(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	// A valid token + resolvable ENI is still refused once X-Forwarded-For is set.
	token := issueToken(t, h)
	req := httptest.NewRequest(http.MethodGet, "http://"+MetaDataServerIP+prefixMetaData+"instance-id", nil)
	req.RemoteAddr = testIP + ":50000"
	req.Header.Set(hdrToken, token)
	req.Header.Set(hdrForwardedFor, "10.0.0.99")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Token issuance is refused too.
	put := httptest.NewRequest(http.MethodPut, "http://"+MetaDataServerIP+pathToken, nil)
	put.RemoteAddr = testIP + ":50000"
	put.Header.Set(hdrTokenTTL, "60")
	put.Header.Set(hdrForwardedFor, "10.0.0.99")
	putRec := httptest.NewRecorder()
	h.ServeHTTP(putRec, put)
	assert.Equal(t, http.StatusForbidden, putRec.Code)
}

func TestHTTP_ENIMissReturns404(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: nil}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), nil)

	// Token PUT and metadata GET both 404 when the tap maps to no ENI.
	req := httptest.NewRequest(http.MethodPut, "http://"+MetaDataServerIP+pathToken, nil)
	req.RemoteAddr = testIP + ":50000"
	req.Header.Set(hdrTokenTTL, "60")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ----- version discovery -------------------------------------------------

// GET / returns the advertised version list (token-gated, IMDSv2-only).
func TestHTTP_RootVersionListing(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, "/", token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "2021-07-15\nlatest", rec.Body.String())
}

// Version discovery is not a tokenless side channel: GET / without a token is 401.
func TestHTTP_RootVersionListingTokenless401(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	rec := get(t, h, "/", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, rec.Body.String())
}

// GET /latest lists the top-level tree.
func TestHTTP_LatestListing(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, "/latest", token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "dynamic\nmeta-data\nuser-data", rec.Body.String())
}

// Any dated API-version prefix aliases to the /latest tree — including a version
// cloud-init probes (2021-03-23) that we do not advertise — while a non-date
// first segment is not rewritten and stays 404. The dated token PUT works too.
func TestHTTP_DatedVersionAlias(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	// PUT /<date>/api/token issues a token like /latest/api/token does.
	put := httptest.NewRequest(http.MethodPut, "http://"+MetaDataServerIP+"/2021-07-15/api/token", nil)
	put.RemoteAddr = testIP + ":50000"
	put.Header.Set(hdrTokenTTL, "60")
	putRec := httptest.NewRecorder()
	h.ServeHTTP(putRec, put)
	require.Equal(t, http.StatusOK, putRec.Code)
	token := putRec.Body.String()
	require.NotEmpty(t, token)

	want := get(t, h, prefixMetaData+"instance-id", token).Body.String()

	// Advertised and non-advertised dated versions both resolve to /latest.
	for _, date := range []string{"2021-07-15", "2021-03-23"} {
		rec := get(t, h, "/"+date+"/meta-data/instance-id", token)
		assert.Equal(t, http.StatusOK, rec.Code, "date=%s", date)
		assert.Equal(t, want, rec.Body.String(), "date=%s", date)
	}

	// A non-date first segment is left alone and 404s.
	rec := get(t, h, "/bogus/meta-data/instance-id", token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ----- dynamic instance-identity -----------------------------------------

// /latest/dynamic lists instance-identity/; /latest/dynamic/instance-identity
// advertises only document (the signed forms are deferred).
func TestHTTP_DynamicListings(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	cases := []struct{ path, want string }{
		{prefixDynamic, "instance-identity/"},
		{prefixDynamic + "/", "instance-identity/"},
		{pathIdentityDir, "document"},
		{pathIdentityDir + "/", "document"},
	}
	for _, c := range cases {
		rec := get(t, h, c.path, token)
		assert.Equal(t, http.StatusOK, rec.Code, "path=%s", c.path)
		assert.Equal(t, c.want, rec.Body.String(), "path=%s", c.path)
	}
}

// The unsigned identity document resolves every field from eni + instance facts;
// fields Spinifex does not model (billingProducts, kernelId, ...) are JSON null.
func TestHTTP_InstanceIdentityDocument(t *testing.T) {
	pending := time.Date(2026, 6, 18, 1, 2, 3, 0, time.UTC)
	res := &fakeResolver{
		eni:  testENI(),
		inst: &instanceFacts{instanceType: "t3.micro", imageID: "ami-12345", architecture: "x86_64", pendingTime: pending},
	}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)

	rec := get(t, h, pathIdentityDocument, token)
	require.Equal(t, http.StatusOK, rec.Code)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	for k, want := range map[string]any{
		"accountId":        "111122223333",
		"architecture":     "x86_64",
		"availabilityZone": "ap-southeast-2a",
		"region":           "ap-southeast-2",
		"imageId":          "ami-12345",
		"instanceId":       "i-0123456789",
		"instanceType":     "t3.micro",
		"privateIp":        "10.0.1.5",
		"pendingTime":      "2026-06-18T01:02:03Z",
		"version":          "2017-09-30",
	} {
		assert.Equal(t, want, doc[k], "field=%s", k)
	}

	// Unmodelled fields marshal to JSON null, not absent and not "".
	for _, k := range []string{"billingProducts", "devpayProductCodes", "marketplaceProductCodes", "kernelId", "ramdiskId"} {
		v, ok := doc[k]
		assert.True(t, ok, "field=%s present", k)
		assert.Nil(t, v, "field=%s null", k)
	}
}

// An instance that is no longer visible (terminating/invisible) is a 404, not a
// document with empty fields.
func TestHTTP_InstanceIdentityDocumentInvisible404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: nil}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, pathIdentityDocument, token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ----- metadata surface --------------------------------------------------

func TestHTTP_MetadataPaths(t *testing.T) {
	res := &fakeResolver{
		eni: testENI(),
		inst: &instanceFacts{
			instanceType:   "t3.micro",
			imageID:        "ami-12345",
			reservationID:  "r-0abc123",
			amiLaunchIndex: 3,
			userData:       []byte("#!/bin/sh\necho hi"),
		},
		sgNames: map[string]string{"sg-1": "web-sg", "sg-2": "db-sg"},
	}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)

	cases := []struct{ path, want string }{
		{prefixMetaData + "instance-id", "i-0123456789"},
		{prefixMetaData + "instance-type", "t3.micro"},
		{prefixMetaData + "ami-id", "ami-12345"},
		{prefixMetaData + "ami-launch-index", "3"},
		{prefixMetaData + "reservation-id", "r-0abc123"},
		{prefixMetaData + "instance-life-cycle", "on-demand"},
		{prefixMetaData + "local-ipv4", "10.0.1.5"},
		{prefixMetaData + "public-ipv4", "203.0.113.7"},
		{prefixMetaData + "public-hostname", "203.0.113.7"},
		{prefixMetaData + "mac", "02:11:22:33:44:55"},
		{prefixMetaData + "placement/availability-zone", "ap-southeast-2a"},
		{prefixMetaData + "placement/region", "ap-southeast-2"},
		{prefixMetaData + "security-groups", "web-sg\ndb-sg"},
		{prefixMetaData + "hostname", "ip-10-0-1-5.ap-southeast-2.compute.internal"},
		{prefixMetaData + "local-hostname", "ip-10-0-1-5.ap-southeast-2.compute.internal"},
		{prefixMetaData + "services", "domain\npartition"},
		{prefixMetaData + "services/", "domain\npartition"},
		{prefixMetaData + "services/domain", "amazonaws.com"},
		{prefixMetaData + "services/partition", "aws"},
		{pathUserData, "#!/bin/sh\necho hi"},
	}
	for _, c := range cases {
		rec := get(t, h, c.path, token)
		assert.Equal(t, http.StatusOK, rec.Code, "path=%s", c.path)
		assert.Equal(t, c.want, rec.Body.String(), "path=%s", c.path)
	}
}

// An instance with no public IP has no public hostname or public IPv4: both leaves
// 404, not an empty 200, matching real EC2 and the meta-data/ listing that omits them.
func TestHTTP_PublicFieldsAbsent404(t *testing.T) {
	eni := testENI()
	eni.publicIP = ""
	svc, _ := newTestService(&fakeResolver{eni: eni}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), eni)
	token := issueToken(t, h)
	for _, leaf := range []string{"public-hostname", "public-ipv4"} {
		rec := get(t, h, prefixMetaData+leaf, token)
		assert.Equal(t, http.StatusNotFound, rec.Code, "leaf=%s", leaf)
	}
}

// instance-life-cycle is "spot" only for a spot-launched instance; it defaults to
// "on-demand" for on-demand instances and — crucially — for a resolution miss or
// error, since the leaf is advertised unconditionally and a 404 would break the crawl.
func TestHTTP_InstanceLifecycle(t *testing.T) {
	cases := []struct {
		name string
		res  *fakeResolver
		want string
	}{
		{"Spot", &fakeResolver{eni: testENI(), inst: &instanceFacts{lifecycleType: "spot"}}, "spot"},
		{"OnDemand", &fakeResolver{eni: testENI(), inst: &instanceFacts{}}, "on-demand"},
		{"ResolutionMiss", &fakeResolver{eni: testENI()}, "on-demand"},
		{"ResolutionError", &fakeResolver{eni: testENI(), instErr: errors.New("backend down")}, "on-demand"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, _ := newTestService(c.res, &fakeIAM{}, &fakeAssumer{})
			h := withTapENI(svc.httpHandler(), testENI())
			token := issueToken(t, h)
			rec := get(t, h, prefixMetaData+"instance-life-cycle", token)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, c.want, rec.Body.String())
		})
	}
}

// A never-interrupted spot instance has no scheduled action, so spot/instance-action
// and spot/termination-time 404 (the dispatch default) — a 200 body would trigger
// interruption handling in pollers like the AWS Node Termination Handler.
func TestHTTP_SpotPathsReturn404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{lifecycleType: "spot"}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	for _, leaf := range []string{"spot/instance-action", "spot/termination-time"} {
		rec := get(t, h, prefixMetaData+leaf, token)
		assert.Equal(t, http.StatusNotFound, rec.Code, "leaf=%s", leaf)
	}
}

// The meta-data root lists every served child, alphabetically, to match AWS. With
// no instance profile attached, iam/ is omitted entirely — real EC2 omits it so
// cloud-init never descends into a subtree whose leaves would 404 and fail the crawl.
func TestHTTP_DirectoryListing(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{keyName: "e2e-key"}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, pathMetaDataRoot, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	want := strings.Join([]string{
		"ami-id",
		"ami-launch-index",
		"hostname",
		"instance-id",
		"instance-life-cycle",
		"instance-type",
		"local-hostname",
		"local-ipv4",
		"mac",
		"network/",
		"placement/",
		"public-hostname",
		"public-ipv4",
		"public-keys/",
		"reservation-id",
		"security-groups",
		"services/",
	}, "\n")
	assert.Equal(t, want, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "iam/")
}

// With an instance profile attached, iam/ appears in the listing (between hostname
// and instance-id) so cloud-init can fetch the role credentials.
func TestHTTP_DirectoryListingWithProfile(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{keyName: "e2e-key", iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile"}}
	svc, _ := newTestService(res, &fakeIAM{profile: profileFixture()}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, pathMetaDataRoot, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	want := strings.Join([]string{
		"ami-id",
		"ami-launch-index",
		"hostname",
		"iam/",
		"instance-id",
		"instance-life-cycle",
		"instance-type",
		"local-hostname",
		"local-ipv4",
		"mac",
		"network/",
		"placement/",
		"public-hostname",
		"public-ipv4",
		"public-keys/",
		"reservation-id",
		"security-groups",
		"services/",
	}, "\n")
	assert.Equal(t, want, rec.Body.String())
}

// A private-subnet instance (no public IP) omits public-hostname and public-ipv4 from
// the listing so cloud-init never fetches the 404ing leaves and falls back to
// DataSourceNone — the regression that left private VMs keyless. public-keys/ stays:
// it tracks the key pair, not the public IP.
func TestHTTP_DirectoryListingNoPublicIP(t *testing.T) {
	eni := testENI()
	eni.publicIP = ""
	res := &fakeResolver{eni: eni, inst: &instanceFacts{keyName: "e2e-key"}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), eni)
	token := issueToken(t, h)
	rec := get(t, h, pathMetaDataRoot, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "public-hostname")
	assert.NotContains(t, body, "public-ipv4")
	assert.Contains(t, body, "public-keys/")
}

// An instance launched without a key pair omits public-keys/, whose leaf would 404.
func TestHTTP_DirectoryListingNoKeyPair(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, pathMetaDataRoot, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "public-keys/")
}

func TestHTTP_UserDataAbsent404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, pathUserData, token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHTTP_OutOfScopePaths404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	for _, p := range []string{
		pathIdentityDir + "/pkcs7",                               // signed forms stay deferred until the signing key lands
		pathIdentityDir + "/rsa2048",                             //
		pathIdentityDir + "/signature",                           //
		prefixNetworkMacs + testENI().mac + "/ipv4-associations", // IPv6/associations stay deferred
		prefixNetworkMacs + testENI().mac + "/ipv6s",             //
		prefixNetworkMacs + "02:00:00:00:00:00/subnet-id",        // a MAC that is not the caller's
		prefixMetaData + "nonsense",
	} {
		rec := get(t, h, p, token)
		assert.Equal(t, http.StatusNotFound, rec.Code, "path=%s", p)
	}
}

// ----- network interfaces ------------------------------------------------

const (
	testSubnetCIDR = "10.0.1.0/24"
	testVPCCIDR    = "10.0.0.0/16"
)

// macPath builds a /network/interfaces/macs/<mac>[/<key>] request path.
func macPath(mac, key string) string {
	p := prefixNetworkMacs + mac
	if key != "" {
		p += "/" + key
	}
	return p
}

// The intermediate directory listings walk down to the caller's single MAC.
func TestHTTP_NetworkInterfacesListings(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	mac := testENI().mac
	cases := []struct{ path, want string }{
		{prefixMetaData + "network", "interfaces/"},
		{prefixMetaData + "network/", "interfaces/"},
		{prefixMetaData + "network/interfaces", "macs/"},
		{prefixMetaData + "network/interfaces/", "macs/"},
		{prefixMetaData + "network/interfaces/macs", mac + "/"},
		{prefixMetaData + "network/interfaces/macs/", mac + "/"},
	}
	for _, c := range cases {
		rec := get(t, h, c.path, token)
		assert.Equal(t, http.StatusOK, rec.Code, "path=%s", c.path)
		assert.Equal(t, c.want, rec.Body.String(), "path=%s", c.path)
	}
}

// macs/<mac> and macs/<mac>/ list exactly the leaves served — CIDR and public keys
// included only when they resolve, so cloud-init's crawl never lists a key that 404s.
func TestHTTP_NetworkInterfaceMacDirListing(t *testing.T) {
	res := &fakeResolver{eni: testENI(), subnetCIDR: testSubnetCIDR, vpcCIDR: testVPCCIDR}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	want := strings.Join([]string{
		"device-number",
		"interface-id",
		"local-hostname",
		"local-ipv4s",
		"mac",
		"owner-id",
		"public-hostname",
		"public-ipv4s",
		"security-group-ids",
		"security-groups",
		"subnet-id",
		"subnet-ipv4-cidr-block",
		"vpc-id",
		"vpc-ipv4-cidr-block",
		"vpc-ipv4-cidr-blocks",
	}, "\n")
	mac := testENI().mac
	for _, p := range []string{macPath(mac, ""), macPath(mac, "") + "/"} {
		rec := get(t, h, p, token)
		assert.Equal(t, http.StatusOK, rec.Code, "path=%s", p)
		assert.Equal(t, want, rec.Body.String(), "path=%s", p)
	}
}

// Every served leaf resolves from eni + resolver facts.
func TestHTTP_NetworkInterfaceLeaves(t *testing.T) {
	res := &fakeResolver{
		eni:        testENI(),
		sgNames:    map[string]string{"sg-1": "web-sg", "sg-2": "db-sg"},
		subnetCIDR: testSubnetCIDR,
		vpcCIDR:    testVPCCIDR,
	}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	mac := testENI().mac
	cases := []struct{ key, want string }{
		{"mac", mac},
		{"device-number", "0"},
		{"interface-id", "eni-aaa"},
		{"owner-id", "111122223333"},
		{"subnet-id", "subnet-1"},
		{"vpc-id", testVPC},
		{"local-ipv4s", testIP},
		{"local-hostname", "ip-10-0-1-5.ap-southeast-2.compute.internal"},
		{"security-group-ids", "sg-1\nsg-2"},
		{"security-groups", "web-sg\ndb-sg"},
		{"subnet-ipv4-cidr-block", testSubnetCIDR},
		{"vpc-ipv4-cidr-block", testVPCCIDR},
		{"vpc-ipv4-cidr-blocks", testVPCCIDR},
		{"public-ipv4s", "203.0.113.7"},
		{"public-hostname", "203.0.113.7"},
	}
	for _, c := range cases {
		rec := get(t, h, macPath(mac, c.key), token)
		assert.Equal(t, http.StatusOK, rec.Code, "key=%s", c.key)
		assert.Equal(t, c.want, rec.Body.String(), "key=%s", c.key)
	}
}

// With no public IP, public-ipv4s and public-hostname 404 and drop from the listing.
func TestHTTP_NetworkInterfacePublicAbsent(t *testing.T) {
	eni := testENI()
	eni.publicIP = ""
	res := &fakeResolver{eni: eni, subnetCIDR: testSubnetCIDR, vpcCIDR: testVPCCIDR}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), eni)
	token := issueToken(t, h)

	listing := get(t, h, macPath(eni.mac, ""), token).Body.String()
	assert.NotContains(t, listing, "public-ipv4s")
	assert.NotContains(t, listing, "public-hostname")

	for _, key := range []string{"public-ipv4s", "public-hostname"} {
		rec := get(t, h, macPath(eni.mac, key), token)
		assert.Equal(t, http.StatusNotFound, rec.Code, "key=%s", key)
	}
}

// On a CIDR miss the CIDR leaves 404 and drop from the listing; a resolver error
// is a 500 on both the leaf and the listing, never an empty/dropped CIDR a guest
// would mis-render into its network config.
func TestHTTP_NetworkInterfaceCidrMiss(t *testing.T) {
	res := &fakeResolver{eni: testENI()} // no canned CIDRs → miss
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	mac := testENI().mac

	listing := get(t, h, macPath(mac, ""), token).Body.String()
	assert.NotContains(t, listing, "subnet-ipv4-cidr-block")
	assert.NotContains(t, listing, "vpc-ipv4-cidr-block")
	for _, key := range []string{"subnet-ipv4-cidr-block", "vpc-ipv4-cidr-block", "vpc-ipv4-cidr-blocks"} {
		rec := get(t, h, macPath(mac, key), token)
		assert.Equal(t, http.StatusNotFound, rec.Code, "key=%s", key)
	}

	errRes := &fakeResolver{eni: testENI(), subnetErr: errors.New("kv unavailable"), vpcErr: errors.New("kv unavailable")}
	errSvc, _ := newTestService(errRes, &fakeIAM{}, &fakeAssumer{})
	errH := withTapENI(errSvc.httpHandler(), testENI())
	errToken := issueToken(t, errH)
	for _, key := range []string{"subnet-ipv4-cidr-block", "vpc-ipv4-cidr-block", "vpc-ipv4-cidr-blocks"} {
		rec := get(t, errH, macPath(mac, key), errToken)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, "key=%s", key)
	}
	listingRec := get(t, errH, macPath(mac, ""), errToken)
	assert.Equal(t, http.StatusInternalServerError, listingRec.Code, "listing under resolver error")
}

// A MAC that is not the caller's is 404: the per-tap responder only serves its own ENI.
func TestHTTP_NetworkInterfaceForeignMac404(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, macPath("02:00:00:00:00:00", "subnet-id"), token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ----- public keys -------------------------------------------------------

func TestHTTP_PublicKeys(t *testing.T) {
	const material = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	pk := &fakePublicKeys{material: material}
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{keyName: "my-key"}}
	svc := newPubKeyTestService(res, pk)
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)

	// Directory listings — served from the launch key name, no material RPC.
	listings := []struct{ path, want string }{
		{prefixPublicKeys, "0=my-key"},           // /public-keys/
		{pathPublicKeysDir, "0=my-key"},          // /public-keys (no trailing slash)
		{prefixPublicKeys + "0/", "openssh-key"}, // /public-keys/0/
		{prefixPublicKeys + "0", "openssh-key"},  // /public-keys/0 (no trailing slash)
	}
	for _, c := range listings {
		rec := get(t, h, c.path, token)
		assert.Equal(t, http.StatusOK, rec.Code, "path=%s", c.path)
		assert.Equal(t, c.want, rec.Body.String(), "path=%s", c.path)
	}
	assert.Equal(t, 0, pk.calls, "directory listings must not invoke the material RPC")

	// Material leaf — the single OpenSSH line plus exactly one trailing newline.
	rec := get(t, h, prefixPublicKeys+"0/openssh-key", token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, material+"\n", rec.Body.String())
	assert.Equal(t, 1, pk.calls)
}

func TestHTTP_PublicKeysNoKey404(t *testing.T) {
	pk := &fakePublicKeys{}
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}} // no key pair bound
	svc := newPubKeyTestService(res, pk)
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	for _, p := range []string{
		prefixPublicKeys,
		pathPublicKeysDir,
		prefixPublicKeys + "0/",
		prefixPublicKeys + "0/openssh-key",
	} {
		rec := get(t, h, p, token)
		assert.Equal(t, http.StatusNotFound, rec.Code, "path=%s", p)
	}
	assert.Equal(t, 0, pk.calls, "a no-key instance must never reach the material RPC")
}

func TestHTTP_PublicKeysUnknownIndex404(t *testing.T) {
	pk := &fakePublicKeys{material: "ssh-ed25519 AAAA"}
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{keyName: "my-key"}}
	svc := newPubKeyTestService(res, pk)
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	for _, p := range []string{
		prefixPublicKeys + "1",
		prefixPublicKeys + "1/openssh-key",
		prefixPublicKeys + "0/nonsense",
	} {
		rec := get(t, h, p, token)
		assert.Equal(t, http.StatusNotFound, rec.Code, "path=%s", p)
	}
	assert.Equal(t, 0, pk.calls, "only index 0's openssh-key leaf may fetch material")
}

// A deleted key (NoSuchKey, surfaced as InvalidKeyPair.NotFound) is definitive
// absence → 404; any other backend fault is 500. A 404 tells cloud-init the
// instance has no key and it boots keyless without retry, so a transient fault
// must never collapse to 404 (silent-lockout regression guard).
func TestHTTP_PublicKeysMaterialDeleted404(t *testing.T) {
	pk := &fakePublicKeys{err: errors.New(awserrors.ErrorInvalidKeyPairNotFound)}
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{keyName: "my-key"}}
	svc := newPubKeyTestService(res, pk)
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, prefixPublicKeys+"0/openssh-key", token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHTTP_PublicKeysMaterialBackendError500(t *testing.T) {
	pk := &fakePublicKeys{err: errors.New("rpc timeout")}
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{keyName: "my-key"}}
	svc := newPubKeyTestService(res, pk)
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, prefixPublicKeys+"0/openssh-key", token)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// A backend failure resolving the instance (which carries the key name) must be
// 500 on the listing path too, not a 404 that masks the fault as "no key".
func TestHTTP_PublicKeysInstanceBackendError500(t *testing.T) {
	pk := &fakePublicKeys{}
	res := &fakeResolver{eni: testENI(), instErr: errors.New("backend unavailable")}
	svc := newPubKeyTestService(res, pk)
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, prefixPublicKeys, token)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ----- IAM / credentials -------------------------------------------------

func profileFixture() *handlers_iam.InstanceProfile {
	return &handlers_iam.InstanceProfile{
		InstanceProfileName: "app-profile",
		InstanceProfileID:   "AIPAEXAMPLE",
		AccountID:           "111122223333",
		ARN:                 "arn:aws:iam::111122223333:instance-profile/app-profile",
		RoleName:            "app-role",
	}
}

func TestHTTP_IAMInfo(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile"}}
	svc, _ := newTestService(res, &fakeIAM{profile: profileFixture()}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)

	rec := get(t, h, prefixMetaData+"iam/info", token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "arn:aws:iam::111122223333:instance-profile/app-profile")
	assert.Contains(t, rec.Body.String(), "AIPAEXAMPLE")
}

func TestHTTP_IAMInfoNoProfile404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}} // no profile ARN
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, prefixMetaData+"iam/info", token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// With an instance profile attached, the iam/ directory lists its children.
func TestHTTP_IAMDir(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile"}}
	svc, _ := newTestService(res, &fakeIAM{profile: profileFixture()}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	for _, path := range []string{prefixMetaData + "iam", prefixMetaData + "iam/"} {
		rec := get(t, h, path, token)
		assert.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, "info\nsecurity-credentials/", rec.Body.String(), path)
	}
}

// With no instance profile, the iam/ directory itself 404s — matching real EC2, so
// cloud-init never descends and never trips on a 404ing iam/info that fails its crawl.
func TestHTTP_IAMDirNoProfile404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}} // no profile ARN
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	for _, path := range []string{prefixMetaData + "iam", prefixMetaData + "iam/"} {
		rec := get(t, h, path, token)
		assert.Equal(t, http.StatusNotFound, rec.Code, path)
	}
}

func TestHTTP_SecurityCredentialsList(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile"}}
	svc, _ := newTestService(res, &fakeIAM{profile: profileFixture()}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, pathSecurityCredsDir, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "app-role", rec.Body.String())
}

func TestHTTP_SecurityCredentialsListNoRoleEmpty(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, pathSecurityCredsDir, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
}

// A backend failure resolving the instance must surface as 500, never an empty
// 200 — otherwise a transient hiccup is indistinguishable from "instance has no
// role" and the SDK silently proceeds unauthenticated.
func TestHTTP_SecurityCredentialsListBackendError500(t *testing.T) {
	res := &fakeResolver{eni: testENI(), instErr: errors.New("backend unavailable")}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, pathSecurityCredsDir, token)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// A profile deleted out from under a running instance (the ARN dereferences to
// NoSuchEntity) is "no instance role", not a backend fault: iam/info 404s, the
// creds list is an empty 200, and the role path 404s — matching AWS. Only a
// genuine backend error (TestHTTP_*BackendError500) is a 500.
func TestHTTP_IAMInfoDeletedProfile404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/gone"}}
	svc, _ := newTestService(res, &fakeIAM{profileErr: errors.New(awserrors.ErrorIAMNoSuchEntity)}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, prefixMetaData+"iam/info", token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHTTP_SecurityCredentialsListDeletedProfileEmpty(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/gone"}}
	svc, _ := newTestService(res, &fakeIAM{profileErr: errors.New(awserrors.ErrorIAMNoSuchEntity)}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, pathSecurityCredsDir, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestHTTP_RoleCredentialsDeletedProfile404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/gone"}}
	svc, _ := newTestService(res, &fakeIAM{profileErr: errors.New(awserrors.ErrorIAMNoSuchEntity)}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, prefixSecurityCreds+"app-role", token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHTTP_IAMInfoBackendError500(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile"}}
	svc, _ := newTestService(res, &fakeIAM{profileErr: errors.New("iam unavailable")}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, prefixMetaData+"iam/info", token)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestHTTP_RoleCredentialsBackendError500(t *testing.T) {
	res := &fakeResolver{eni: testENI(), instErr: errors.New("backend unavailable")}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)
	rec := get(t, h, prefixSecurityCreds+"app-role", token)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestHTTP_RoleCredentials(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile"}}
	assumer := &fakeAssumer{out: &sts.AssumeRoleOutput{Credentials: &sts.Credentials{
		AccessKeyId:     aws.String("ASIAEXAMPLE"),
		SecretAccessKey: aws.String("secret"),
		SessionToken:    aws.String("token"),
		Expiration:      aws.Time(now.Add(time.Hour)),
	}}}
	svc, _ := newTestService(res, &fakeIAM{profile: profileFixture(), roleARN: "arn:aws:iam::111122223333:role/app-role"}, assumer)
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)

	rec := get(t, h, prefixSecurityCreds+"app-role", token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "ASIAEXAMPLE")
	assert.Contains(t, rec.Body.String(), `"Code":"Success"`)
}

func TestHTTP_RoleCredentialsWrongRole404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile"}}
	svc, _ := newTestService(res, &fakeIAM{profile: profileFixture(), roleARN: "arn:aws:iam::111122223333:role/app-role"}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())
	token := issueToken(t, h)

	// AWS only accepts the actual role name, never the profile name.
	rec := get(t, h, prefixSecurityCreds+"app-profile", token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
