package awsgw_test

import (
	"encoding/xml"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSigV4Auth_NoAuthorizationHeader(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "MissingAuthenticationToken") {
		t.Errorf("Expected MissingAuthenticationToken error, got: %s", string(body))
	}
}

func TestSigV4Auth_MalformedHeader(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	testCases := []struct {
		name       string
		authHeader string
	}{
		{"empty prefix", "InvalidPrefix Credential=test"},
		{"missing parts", "AWS4-HMAC-SHA256 Credential=test"},
		{"invalid credential format", "AWS4-HMAC-SHA256 Credential=a/b/c, SignedHeaders=host, Signature=sig"},
		{"missing SignedHeaders prefix", "AWS4-HMAC-SHA256 Credential=a/b/c/d/aws4_request, Headers=host, Signature=sig"},
		{"missing Signature prefix", "AWS4-HMAC-SHA256 Credential=a/b/c/d/aws4_request, SignedHeaders=host, Sig=abc"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = "localhost:9999"
			req.Header.Set("Authorization", tc.authHeader)
			req.Header.Set("X-Amz-Date", time.Now().UTC().Format("20060102T150405Z"))

			resp := doRequest(handler, req)

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("Expected status 400, got %d", resp.StatusCode)
			}

			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "IncompleteSignature") {
				t.Errorf("Expected IncompleteSignature error, got: %s", string(body))
			}
		})
	}
}

func TestSigV4Auth_InvalidAccessKey(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, "INVALID_ACCESS_KEY", gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "InvalidClientTokenId") {
		t.Errorf("Expected InvalidClientTokenId error, got: %s", string(body))
	}
}

func TestSigV4Auth_InvalidSignature(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, "WRONG_SECRET_KEY")

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SignatureDoesNotMatch") {
		t.Errorf("Expected SignatureDoesNotMatch error, got: %s", string(body))
	}
}

func TestSigV4Auth_InactiveAccessKey(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{
			gwfix.AccessKey: {
				AccessKeyID:     gwfix.AccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "root",
				Status:          "Inactive",
			},
		},
	}

	gw := withEC2Verifier(gwfix.WithDeps(&pipeline.Config{
		DisableLogging: true,
		Region:         gwfix.Region,
	}, gwfix.Deps{IAM: mockSvc}))

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403 for inactive key, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "InvalidClientTokenId") {
		t.Errorf("Expected InvalidClientTokenId error, got: %s", string(body))
	}
}

func TestSigV4Auth_DecryptFailure(t *testing.T) {
	// Secret encrypted under a rotated master key — unverifiable credential must
	// return 403 InvalidClientTokenId, not 500, so the client re-authenticates.
	otherKey, err := iamdomain.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey() error: %v", err)
	}
	encryptedWithOther, err := iamdomain.EncryptSecret(gwfix.SecretKey, otherKey)
	if err != nil {
		t.Fatalf("EncryptSecret() error: %v", err)
	}

	mockSvc := &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{
			gwfix.AccessKey: {
				AccessKeyID:     gwfix.AccessKey,
				SecretAccessKey: encryptedWithOther, // encrypted with wrong key
				UserName:        "root",
				Status:          "Active",
			},
		},
	}

	gw := withEC2Verifier(gwfix.WithDeps(&pipeline.Config{
		DisableLogging: true,
		Region:         gwfix.Region,
	}, gwfix.Deps{IAM: mockSvc}))

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403 for decrypt failure, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), awserrors.ErrorInvalidClientTokenId) {
		t.Errorf("Expected InvalidClientTokenId, got: %s", string(body))
	}
}

func TestSigV4Auth_ExpiredTimestamp(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	past := time.Now().UTC().Add(-16 * time.Minute) // exceeds sigv4 15-minute MaxClockSkew
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey, past)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SignatureDoesNotMatch") {
		t.Errorf("Expected SignatureDoesNotMatch error, got: %s", string(body))
	}
}

func TestSigV4Auth_FutureTimestamp(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	future := time.Now().UTC().Add(16 * time.Minute) // exceeds sigv4 15-minute MaxClockSkew
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey, future)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SignatureDoesNotMatch") {
		t.Errorf("Expected SignatureDoesNotMatch error, got: %s", string(body))
	}
}

func TestSigV4Auth_MissingXAmzDate(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)
	req.Header.Del("X-Amz-Date")

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "IncompleteSignature") {
		t.Errorf("Expected IncompleteSignature error, got: %s", string(body))
	}
}

func TestSigV4Auth_MalformedTimestamp(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)
	req.Header.Set("X-Amz-Date", "not-a-valid-date")

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "IncompleteSignature") {
		t.Errorf("Expected IncompleteSignature error, got: %s", string(body))
	}
}

// TestSigV4Auth_RequireSignedHeaders verifies that omitting "host" or
// "x-amz-date" from SignedHeaders is rejected with IncompleteSignature before
// signature comparison. Omitting either enables Authorization header replay
// across vhosts or outside the clock-skew window.
func TestSigV4Auth_RequireSignedHeaders(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	rewriteSignedHeaders := func(authHeader, list string) string {
		parts := strings.Split(authHeader, ", ")
		if len(parts) != 3 {
			t.Fatalf("expected 3-part auth header, got %d", len(parts))
		}
		parts[1] = "SignedHeaders=" + list
		return strings.Join(parts, ", ")
	}

	cases := []struct {
		name       string
		signedList string
	}{
		{"missing host", "x-amz-date"},
		{"missing x-amz-date", "host"},
		{"neither present", "content-type"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = "localhost:9999"
			gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)
			req.Header.Set("Authorization",
				rewriteSignedHeaders(req.Header.Get("Authorization"), tc.signedList))

			resp := doRequest(handler, req)

			if resp.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("Expected status 400, got %d, body: %s", resp.StatusCode, string(body))
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "IncompleteSignature") {
				t.Errorf("Expected IncompleteSignature error, got: %s", string(body))
			}
		})
	}
}

// The most important test in this cluster: a genuine signature mismatch on a
// SERVED scope must still return SignatureDoesNotMatch. Nothing about the
// unserved-scope InvalidAction change may make a real signature failure
// unrecognisable.
func TestSigV4Auth_ServedScopeSignatureMismatchStillSignatureDoesNotMatch(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	// Signed with the wrong secret: the credential scope (ec2) is served, so
	// this must fail verification rather than the unserved-service gate.
	gwfix.SignRequestAs(t, req, nil, gwfix.AccessKey, "wrong-secret-entirely", gwfix.Service, time.Now().UTC())

	resp := doRequest(handler, req)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	// ec2 speaks the query-protocol <Response><Errors><Error> shape, not the
	// generic <ErrorResponse><Error> envelope other services use.
	var parsed struct {
		Errors struct {
			Error struct {
				Code string `xml:"Code"`
			} `xml:"Error"`
		} `xml:"Errors"`
	}
	require.NoError(t, xml.Unmarshal(body, &parsed), "body: %s", body)
	assert.Equal(t, awserrors.ErrorSignatureDoesNotMatch, parsed.Errors.Error.Code)
}
