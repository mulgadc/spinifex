package issuer

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTrustAnchor = "arn:aws:rolesanywhere:us-east-1:111122223333:trust-anchor/t1"
	testProfile     = "arn:aws:rolesanywhere:us-east-1:111122223333:profile/p1"
	testRole        = "arn:aws:iam::111122223333:role/R"
)

var testReq = SessionRequest{TrustAnchorARN: testTrustAnchor, ProfileARN: testProfile, RoleARN: testRole}

func TestClient_CreateSession(t *testing.T) {
	cert := fixtureCert(t)
	var gotBody, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotPath, gotAuth = string(b), r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"credentialSet":[{"credentials":{"accessKeyId":"AKIATEST","secretAccessKey":"SECRET","sessionToken":"TOKEN","expiration":"2021-11-03T12:15:00Z"}}],"subjectArn":"x"}`))
	}))
	defer srv.Close()

	c := &Client{
		HTTP:     srv.Client(),
		Endpoint: func(string) string { return srv.URL },
		Now:      func() time.Time { return time.Date(2021, 11, 3, 12, 0, 0, 0, time.UTC) },
	}
	s, err := c.CreateSession(context.Background(), cert, testReq)
	require.NoError(t, err)
	assert.Equal(t, "AKIATEST", s.AccessKeyID)
	assert.Equal(t, "SECRET", s.SecretAccessKey)
	assert.Equal(t, "TOKEN", s.SessionToken)
	assert.Equal(t, time.Date(2021, 11, 3, 12, 15, 0, 0, time.UTC), s.Expiration)
	assert.Equal(t, "/sessions", gotPath)
	assert.Equal(t, fixtureBody, gotBody)
	assert.Contains(t, gotAuth, "Credential=4660/20211103/us-east-1/rolesanywhere/aws4_request")

	// The signature covers the body that was sent.
	digest := sha256.Sum256([]byte(stringToSign("20211103T120000Z", "20211103/us-east-1/rolesanywhere/aws4_request",
		canonicalRequest(strings.TrimPrefix(srv.URL, "http://"), "20211103T120000Z", fixtureCertB64, "/sessions", []byte(gotBody)))))
	sigHex := gotAuth[strings.LastIndex(gotAuth, "Signature=")+len("Signature="):]
	sig, err := hex.DecodeString(sigHex)
	require.NoError(t, err)
	assert.True(t, ecdsa.VerifyASN1(&cert.key.PublicKey, digest[:], sig))
}

func TestClient_CreateSession_ErrorAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amzn-Errortype", "AccessDeniedException:http://internal.amazon.com/coral/")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Untrusted signing certificate"}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Endpoint: func(string) string { return srv.URL }}
	_, err := c.CreateSession(context.Background(), fixtureCert(t), testReq)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 403, apiErr.StatusCode)
	assert.Equal(t, "AccessDeniedException", apiErr.Code)
	assert.Equal(t, "Untrusted signing certificate", apiErr.Message)
	assert.True(t, apiErr.Refused())
	assert.False(t, (&APIError{StatusCode: 429}).Refused())
	assert.False(t, (&APIError{StatusCode: 500}).Refused())
}

func TestClient_CreateSession_MalformedResponses(t *testing.T) {
	for name, body := range map[string]string{
		"empty set":      `{"credentialSet":[]}`,
		"not json":       `nope`,
		"bad expiration": `{"credentialSet":[{"credentials":{"accessKeyId":"a","secretAccessKey":"b","sessionToken":"c","expiration":"soon"}}]}`,
		"missing secret": `{"credentialSet":[{"credentials":{"accessKeyId":"a","sessionToken":"c","expiration":"2021-11-03T12:15:00Z"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			c := &Client{HTTP: srv.Client(), Endpoint: func(string) string { return srv.URL }}
			_, err := c.CreateSession(context.Background(), fixtureCert(t), testReq)
			assert.Error(t, err)
		})
	}
}

func TestClient_CreateSession_ErrorCodeForms(t *testing.T) {
	for name, tt := range map[string]struct{ header, body, want string }{
		"header with uri":      {"AccessDeniedException:http://x/", `{}`, "AccessDeniedException"},
		"type with namespace":  {"", `{"__type":"com.amazon#ValidationException","message":"m"}`, "ValidationException"},
		"code field":           {"", `{"code":"ThrottlingException"}`, "ThrottlingException"},
		"header beats body":    {"ResourceNotFoundException", `{"__type":"Other"}`, "ResourceNotFoundException"},
		"namespace in header":  {"aws.protocoltests#FooError", `{}`, "FooError"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 400, Header: http.Header{}}
			if tt.header != "" {
				resp.Header.Set("X-Amzn-Errortype", tt.header)
			}
			assert.Equal(t, tt.want, apiError(resp, []byte(tt.body)).Code)
		})
	}
}

func TestClient_CreateSession_AcceptsPartitionsAndRolePaths(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"credentialSet":[{"credentials":{"accessKeyId":"a","secretAccessKey":"b","sessionToken":"c","expiration":"2021-11-03T12:15:00Z"}}]}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Endpoint: func(string) string { return srv.URL }, Now: func() time.Time { return testNow }}
	_, err := c.CreateSession(context.Background(), fixtureCert(t), SessionRequest{
		TrustAnchorARN: "arn:aws-us-gov:rolesanywhere:us-gov-west-1:111122223333:trust-anchor/t1",
		ProfileARN:     "arn:aws-us-gov:rolesanywhere:us-gov-west-1:111122223333:profile/p1",
		RoleARN:        "arn:aws-us-gov:iam::111122223333:role/team/ops/My_Role+x=y,z.w@v-1",
	})
	require.NoError(t, err)
	assert.Contains(t, gotHost, "/us-gov-west-1/rolesanywhere/aws4_request")
	assert.Equal(t, "https://rolesanywhere.cn-north-1.amazonaws.com.cn", DefaultEndpoint("cn-north-1"))
	assert.Equal(t, "https://rolesanywhere.us-east-1.amazonaws.com", DefaultEndpoint("us-east-1"))
}

func TestClient_CreateSession_RejectsBadARNs(t *testing.T) {
	c := &Client{HTTP: &http.Client{Transport: failTransport{t}}}
	for _, r := range []SessionRequest{
		{TrustAnchorARN: "arn:aws-iso:rolesanywhere:us-iso-east-1:111122223333:trust-anchor/t", ProfileARN: testProfile, RoleARN: testRole},
		{TrustAnchorARN: testTrustAnchor, ProfileARN: "profile", RoleARN: testRole},
		{TrustAnchorARN: testTrustAnchor, ProfileARN: testProfile, RoleARN: "role"},
		{TrustAnchorARN: "arn:aws:rolesanywhere:us-east-1.evil.com/x:111122223333:trust-anchor/t", ProfileARN: testProfile, RoleARN: testRole},
	} {
		_, err := c.CreateSession(context.Background(), fixtureCert(t), r)
		assert.Error(t, err)
	}
}

type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("no request must be sent for an invalid ARN")
	return nil, fmt.Errorf("unexpected request")
}

func TestSession_NeverPrintsCredentials(t *testing.T) {
	s := Session{AccessKeyID: "AKIATEST", SecretAccessKey: "SECRET", SessionToken: "TOKEN", Expiration: testNow}
	for _, out := range []string{
		fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprint(&s),
		slogText(s),
	} {
		assert.NotContains(t, out, "SECRET")
		assert.NotContains(t, out, "TOKEN")
		assert.NotContains(t, out, "AKIATEST")
	}
	_, err := s.MarshalJSON()
	assert.Error(t, err)
}

func slogText(s Session) string {
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("x", "session", s)
	return b.String()
}
