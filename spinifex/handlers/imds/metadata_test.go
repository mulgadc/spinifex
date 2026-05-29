package handlers_imds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----- fakes -------------------------------------------------------------

type fakeResolver struct {
	eni     *eniFacts
	eniErr  error
	inst    *instanceFacts
	instErr error
}

func (f *fakeResolver) resolveENI(_, _ string) (*eniFacts, error) { return f.eni, f.eniErr }
func (f *fakeResolver) resolveInstance(_ *eniFacts) (*instanceFacts, error) {
	return f.inst, f.instErr
}

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

// newTestService builds an IMDSServiceImpl with injected fakes and a fixed
// clock. The bind manager is left nil — the HTTP handler is exercised directly.
func newTestService(res eniResolver, fIAM profileLookup, assumer stsAssumer) (*IMDSServiceImpl, time.Time) {
	now := time.Unix(1_700_000_000, 0).UTC()
	return &IMDSServiceImpl{
		resolver: res,
		tokens:   newTokenStore(),
		creds:    newCredCache(assumer),
		iam:      fIAM,
		now:      func() time.Time { return now },
	}, now
}

// get issues a token-gated GET with the VPC context + source IP a real listener
// would have threaded in, plus the supplied token header.
func get(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+MetaDataServerIP+path, nil)
	req.RemoteAddr = testIP + ":50000"
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyVPCID, testVPC))
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
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyVPCID, testVPC))
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
	h := svc.httpHandler()
	rec := get(t, h, prefixMetaData+"instance-id", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestHTTP_TokenPUTValidation(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := svc.httpHandler()

	for _, ttl := range []string{"", "0", "21601", "notanumber"} {
		req := httptest.NewRequest(http.MethodPut, "http://"+MetaDataServerIP+pathToken, nil)
		req.RemoteAddr = testIP + ":50000"
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyVPCID, testVPC))
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
	h := svc.httpHandler()
	req := httptest.NewRequest(http.MethodGet, "http://"+MetaDataServerIP+pathToken, nil)
	req.RemoteAddr = testIP + ":50000"
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyVPCID, testVPC))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHTTP_ENIMissReturns404(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: nil}, &fakeIAM{}, &fakeAssumer{})
	h := svc.httpHandler()

	// Token PUT and metadata GET both 404 when the source IP maps to no ENI.
	req := httptest.NewRequest(http.MethodPut, "http://"+MetaDataServerIP+pathToken, nil)
	req.RemoteAddr = testIP + ":50000"
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyVPCID, testVPC))
	req.Header.Set(hdrTokenTTL, "60")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ----- metadata surface --------------------------------------------------

func TestHTTP_MetadataPaths(t *testing.T) {
	res := &fakeResolver{
		eni:  testENI(),
		inst: &instanceFacts{instanceType: "t3.micro", imageID: "ami-12345", userData: []byte("#!/bin/sh\necho hi")},
	}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := svc.httpHandler()
	token := issueToken(t, h)

	cases := []struct{ path, want string }{
		{prefixMetaData + "instance-id", "i-0123456789"},
		{prefixMetaData + "instance-type", "t3.micro"},
		{prefixMetaData + "ami-id", "ami-12345"},
		{prefixMetaData + "local-ipv4", "10.0.1.5"},
		{prefixMetaData + "public-ipv4", "203.0.113.7"},
		{prefixMetaData + "mac", "02:11:22:33:44:55"},
		{prefixMetaData + "placement/availability-zone", "ap-southeast-2a"},
		{prefixMetaData + "placement/region", "ap-southeast-2"},
		{prefixMetaData + "security-groups", "sg-1\nsg-2"},
		{prefixMetaData + "hostname", "ip-10-0-1-5.ap-southeast-2.compute.internal"},
		{prefixMetaData + "local-hostname", "ip-10-0-1-5.ap-southeast-2.compute.internal"},
		{pathUserData, "#!/bin/sh\necho hi"},
	}
	for _, c := range cases {
		rec := get(t, h, c.path, token)
		assert.Equal(t, http.StatusOK, rec.Code, "path=%s", c.path)
		assert.Equal(t, c.want, rec.Body.String(), "path=%s", c.path)
	}
}

func TestHTTP_DirectoryListing(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})
	h := svc.httpHandler()
	token := issueToken(t, h)
	rec := get(t, h, pathMetaDataRoot, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "instance-id")
	assert.Contains(t, rec.Body.String(), "iam/")
}

func TestHTTP_UserDataAbsent404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := svc.httpHandler()
	token := issueToken(t, h)
	rec := get(t, h, pathUserData, token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHTTP_OutOfScopePaths404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := svc.httpHandler()
	token := issueToken(t, h)
	for _, p := range []string{
		"/latest/dynamic/instance-identity/document",
		prefixMetaData + "network/interfaces/macs",
		prefixMetaData + "nonsense",
	} {
		rec := get(t, h, p, token)
		assert.Equal(t, http.StatusNotFound, rec.Code, "path=%s", p)
	}
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
	h := svc.httpHandler()
	token := issueToken(t, h)

	rec := get(t, h, prefixMetaData+"iam/info", token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "arn:aws:iam::111122223333:instance-profile/app-profile")
	assert.Contains(t, rec.Body.String(), "AIPAEXAMPLE")
}

func TestHTTP_IAMInfoNoProfile404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}} // no profile ARN
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := svc.httpHandler()
	token := issueToken(t, h)
	rec := get(t, h, prefixMetaData+"iam/info", token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHTTP_SecurityCredentialsList(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile"}}
	svc, _ := newTestService(res, &fakeIAM{profile: profileFixture()}, &fakeAssumer{})
	h := svc.httpHandler()
	token := issueToken(t, h)
	rec := get(t, h, pathSecurityCredsDir, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "app-role", rec.Body.String())
}

func TestHTTP_SecurityCredentialsListNoRoleEmpty(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := svc.httpHandler()
	token := issueToken(t, h)
	rec := get(t, h, pathSecurityCredsDir, token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
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
	h := svc.httpHandler()
	token := issueToken(t, h)

	rec := get(t, h, prefixSecurityCreds+"app-role", token)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "ASIAEXAMPLE")
	assert.Contains(t, rec.Body.String(), `"Code":"Success"`)
}

func TestHTTP_RoleCredentialsWrongRole404(t *testing.T) {
	res := &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile"}}
	svc, _ := newTestService(res, &fakeIAM{profile: profileFixture(), roleARN: "arn:aws:iam::111122223333:role/app-role"}, &fakeAssumer{})
	h := svc.httpHandler()
	token := issueToken(t, h)

	// AWS only accepts the actual role name, never the profile name.
	rec := get(t, h, prefixSecurityCreds+"app-profile", token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
