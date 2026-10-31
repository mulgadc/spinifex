package gateway

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	ecsawsapi "github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	ochreawsapi "github.com/mulgadc/spinifex/spinifex/domains/ochre/awsapi"
	route53awsapi "github.com/mulgadc/spinifex/spinifex/domains/route53/awsapi"
	taggingawsapi "github.com/mulgadc/spinifex/spinifex/domains/tagging/awsapi"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	authlimit "github.com/mulgadc/spinifex/spinifex/ingress/aws/ratelimit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestApp(accessKey, secretKey string) http.Handler {
	encryptedSecret, err := iamdomain.EncryptSecret(secretKey, gwfix.MasterKey)
	if err != nil {
		panic("failed to encrypt test secret: " + err.Error())
	}

	mockSvc := &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{
			accessKey: {
				AccessKeyID:     accessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "root",
				Status:          "Active",
			},
		},
	}

	gw := withRealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})
	withServices(gw, awsapi.NewRegistration(awsapi.Deps{}), ec2Registration(gw))

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	return r
}

// userIDApp signs requests for alice on a real account, so the middleware takes
// the aws:userid branch that a user principal reaches in production.
func userIDApp(t *testing.T, svc *gwfix.IAM) (http.Handler, *string) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	svc.MasterKey = gwfix.MasterKey
	svc.AccessKeys = map[string]*iamdomain.AccessKey{
		gwfix.AccessKey: {
			AccessKeyID:     gwfix.AccessKey,
			SecretAccessKey: encryptedSecret,
			UserName:        "alice",
			AccountID:       "000000000001",
			Status:          "Active",
		},
	}

	gw := withEC2(withRealAuth(&GatewayConfig{DisableLogging: true, Region: gwfix.Region, IAMService: svc}))
	seen := new(string)
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		*seen = callerOf(r).UserID
		w.Write([]byte("OK"))
	})
	return r, seen
}

// aws:userid is resolved once, by the middleware, so every policy check in the
// request evaluates the same value rather than re-reading the user record.
func TestSigV4Auth_StashesUserID(t *testing.T) {
	handler, seen := userIDApp(t, &gwfix.IAM{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "AIDAALICE", *seen)
}

// An IAM fault must fail the request, not authorize it against a context missing
// aws:userid: that silently narrows an Allow and widens a Deny to everything.
func TestSigV4Auth_UserIDDependencyFaultFailsClosed(t *testing.T) {
	handler, _ := userIDApp(t, &gwfix.IAM{
		GetUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return nil, errors.New("nats: no responders available for request")
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), awserrors.ErrorInternalError)
}

func TestSigV4Auth_ValidSignature(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestSigV4Auth_ValidSignatureWithBody(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	body := []byte("Action=DescribeInstances&Version=2016-11-15")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gwfix.SignRequest(t, req, body, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(respBody))
	}
}

func TestSigV4Auth_ValidSignatureWithQueryString(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	queryString := "Action=DescribeInstances&Version=2016-11-15"
	req := httptest.NewRequest(http.MethodGet, "/?"+queryString, nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(respBody))
	}
}

func TestSigV4Auth_NilIAMService(t *testing.T) {
	gw := withEC2(withRealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     nil,
	}))

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Expected status 500 for nil IAM service, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "InternalError") {
		t.Errorf("Expected InternalError, got: %s", string(body))
	}
}

func TestParseAWSQueryArgs_URLDecoding(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		key      string
		expected string
	}{
		{"plain value", "Action=DescribeInstances", "Action", "DescribeInstances"},
		{"encoded slashes", "Device=%2Fdev%2Fsdf", "Device", "/dev/sdf"},
		{"encoded spaces", "Name=my%20volume", "Name", "my volume"},
		{"encoded plus as space", "Name=my+volume", "Name", "my volume"},
		{"no encoding needed", "VolumeId=vol-abc123", "VolumeId", "vol-abc123"},
		{"multiple params", "VolumeId=vol-abc&Device=%2Fdev%2Fsdg", "Device", "/dev/sdg"},
		{"encoded key dot", "Tag%2EKey=Name", "Tag.Key", "Name"},
		{"encoded key and value", "Filter%2E1%2EName=instance-id&Filter%2E1%2EValue=i-abc", "Filter.1.Name", "instance-id"},
		{"key-only encoded", "Tag%2EKey=", "Tag.Key", ""},
		{"key without value", "Tag%2EKey", "Tag.Key", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseAWSQueryArgs(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result[tc.key] != tc.expected {
				t.Errorf("Expected %q for key %q, got %q", tc.expected, tc.key, result[tc.key])
			}
		})
	}
}

func TestSigV4Auth_RequestBodyTooLarge(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	oversizedBody := []byte(strings.Repeat("x", sigv4.MaxPayloadLen+1))
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversizedBody))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gwfix.SignRequest(t, req, oversizedBody, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("Expected status 413, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "RequestEntityTooLarge") {
		t.Errorf("Expected RequestEntityTooLarge error, got: %s", string(body))
	}
}

// A body the gateway cannot read is a transport failure: it answers InternalError
// and leaves the lockout alone, so a flaky link cannot lock out its own IP.
func TestSigV4Auth_BodyReadFailureDoesNotCountTowardLockout(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	for range authlimit.MaxFailures + 1 {
		req := httptest.NewRequest(http.MethodPost, "/", iotest.ErrReader(io.ErrUnexpectedEOF))
		req.Host = "localhost:9999"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		gwfix.SignRequest(t, req, []byte("Action=DescribeInstances"), gwfix.AccessKey, gwfix.SecretKey)

		resp := doRequest(handler, req)
		body, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode, string(body))
		require.Contains(t, string(body), awserrors.ErrorInternalError)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "body-read failures must not lock out the IP")
}

// --- Clock Skew / Replay Protection Tests ---

func TestSigV4Auth_TimestampWithinSkew(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	recent := time.Now().UTC().Add(-14 * time.Minute) // within the 15-minute window
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey, recent)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
	}
}

// --- Clock Skew Boundary Tests ---

func TestSigV4Auth_ClockSkewBoundary(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	testCases := []struct {
		name       string
		offset     time.Duration
		expectPass bool
	}{
		// 14m59s not 15m to absorb test execution time.
		{"just within 15 min past", -(14*time.Minute + 59*time.Second), true},
		{"just beyond 15 min past", -(15*time.Minute + 1*time.Second), false},
		{"just within 15 min future", 14*time.Minute + 59*time.Second, true},
		{"just beyond 15 min future", 15*time.Minute + 1*time.Second, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().UTC().Add(tc.offset)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = "localhost:9999"
			gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey, at)

			resp := doRequest(handler, req)

			if tc.expectPass && resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("Expected 200, got %d, body: %s", resp.StatusCode, string(body))
			}
			if !tc.expectPass && resp.StatusCode != http.StatusForbidden {
				t.Errorf("Expected 403, got %d", resp.StatusCode)
			}
		})
	}
}

// --- writeSigV4Error Response Format Tests ---

// The gateway does not serve S3 — predastore does — so an S3-scoped request is
// rejected on credential scope before any verb or path parsing. Every S3 verb
// therefore takes one path, and all that varies below is the request shape.
func TestSigV4Auth_S3ScopeReturnsFlatS3Error(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	testCases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"PutObject", http.MethodPut, "/bucket/key.txt", []byte("payload")},
		{"ListObjectsV2", http.MethodGet, "/bucket?list-type=2&prefix=a%2F", nil},
		{"CreateBucket", http.MethodPut, "/bucket", nil},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
			req.Host = "localhost:9999"
			gwfix.SignRequestAs(t, req, tc.body, gwfix.AccessKey, gwfix.SecretKey, "s3", time.Now().UTC())

			resp := doRequest(handler, req)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			var parsed struct {
				Code      string `xml:"Code"`
				Message   string `xml:"Message"`
				Resource  string `xml:"Resource"`
				RequestID string `xml:"RequestId"`
			}
			require.NoError(t, xml.Unmarshal(body, &parsed), "body: %s", body)

			// The flat root element is the whole point: an SDK that finds
			// <Response> here reports an empty code and an empty message.
			assert.Equal(t, "Error", xmlRootName(t, body))
			assert.Equal(t, awserrors.ErrorInvalidAction, parsed.Code)
			assert.NotEmpty(t, parsed.Message)
			assert.NotEmpty(t, parsed.RequestID)
			assert.Equal(t, req.URL.Path, parsed.Resource)
		})
	}
}

// --- Context Propagation Tests ---

func TestSigV4Auth_ContextPropagation(t *testing.T) {
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
				UserName:        "alice",
				AccountID:       "123456789012",
				Status:          "Active",
			},
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		caller := callerOf(r)
		identity, accountId, accessKey := caller.Identity, caller.AccountID, reqctx.AccessKeyID(r.Context())
		service := reqctx.Service(r.Context())
		region := reqctx.Region(r.Context())
		fmt.Fprintf(w,
			"identity=%v|accountId=%v|service=%v|region=%v|accessKey=%v",
			identity, accountId, service, region, accessKey,
		)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)
	result := string(body)

	checks := []struct {
		label    string
		expected string
	}{
		{"identity", "identity=alice"},
		{"accountId", "accountId=123456789012"},
		{"service", "service=ec2"},
		{"region", "region=us-east-1"},
		{"accessKey", "accessKey=" + gwfix.AccessKey},
	}
	for _, check := range checks {
		if !strings.Contains(result, check.expected) {
			t.Errorf("Expected %s in response, got: %s", check.expected, result)
		}
	}
}

func TestSigV4Auth_ContextDoesNotLeakBetweenRequests(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	secondKey := "AKIAIOSFODNN7SECOND0"
	encryptedSecret2, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{
			gwfix.AccessKey: {
				AccessKeyID:     gwfix.AccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "alice",
				AccountID:       "111111111111",
				Status:          "Active",
			},
			secondKey: {
				AccessKeyID:     secondKey,
				SecretAccessKey: encryptedSecret2,
				UserName:        "bob",
				AccountID:       "222222222222",
				Status:          "Active",
			},
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		caller := callerOf(r)
		fmt.Fprintf(w, "%v:%v", caller.Identity, caller.AccountID)
	})

	// First request as alice
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Host = "localhost:9999"
	gwfix.SignRequest(t, req1, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp1 := doRequest(r, req1)
	body1, _ := io.ReadAll(resp1.Body)
	if string(body1) != "alice:111111111111" {
		t.Errorf("Request 1: expected alice:111111111111, got %s", string(body1))
	}

	// Second request as bob
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Host = "localhost:9999"
	gwfix.SignRequest(t, req2, nil, secondKey, gwfix.SecretKey)

	resp2 := doRequest(r, req2)
	body2, _ := io.ReadAll(resp2.Body)
	if string(body2) != "bob:222222222222" {
		t.Errorf("Request 2: expected bob:222222222222, got %s", string(body2))
	}
}

// --- LookupAccessKey Error Variants ---

func TestSigV4Auth_LookupAccessKeyUnexpectedError(t *testing.T) {
	// A non-NoSuchEntity error from LookupAccessKey should yield 500 InternalError.
	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     &gwfix.ErrorLookupIAM{Err: errors.New("nats: connection closed")},
	})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Expected status 500 for unexpected lookup error, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "InternalError") {
		t.Errorf("Expected InternalError, got: %s", string(body))
	}
}

// --- Signature Edge Cases ---

func TestSigV4Auth_PathWithSpecialCharacters(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	testCases := []struct {
		name    string
		reqPath string
	}{
		{"encoded space", "/my%20resource"},
		{"nested slashes", "/a/b/c/d"},
		{"trailing slash", "/path/"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.reqPath, nil)
			req.Host = "localhost:9999"
			gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

			resp := doRequest(handler, req)

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
			}
		})
	}
}

func TestSigV4Auth_SignedContentType(t *testing.T) {
	// Verify signature works when content-type is included in signed headers.
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
				Status:          "Active",
			},
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	body := []byte("Action=DescribeInstances")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gwfix.SignRequest(t, req, body, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(respBody))
	}
}

func TestSigV4Auth_EmptyBodyPOST(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	// POST with empty body — payload hash is hash of ""
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestSigV4Auth_MultipartContentType(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	body := []byte("Action=DescribeInstances")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "multipart/form-data; boundary=something")
	gwfix.SignRequest(t, req, body, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200 for multipart content-type, got %d, body: %s", resp.StatusCode, string(respBody))
	}
}

func TestSigV4Auth_QueryStringEdgeCases(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	testCases := []struct {
		name        string
		queryString string
	}{
		{"empty value", "key="},
		{"special chars in value", "Name=hello%20world&Tag=foo%2Fbar"},
		{"duplicate keys", "Filter=a&Filter=b"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/?"+tc.queryString, nil)
			req.Host = "localhost:9999"
			gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

			resp := doRequest(handler, req)

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
			}
		})
	}
}

// --- Policy Enforcement Tests (checkPolicy) ---

// setupPolicyTestHandler creates an http.Handler that authenticates with SigV4 then calls checkPolicy.
func setupPolicyTestHandler(gw *GatewayConfig) http.Handler {
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		if err := gw.checkPolicy(r, "ec2", "RunInstances"); err != nil {
			gw.ErrorHandler(w, r, err)
			return
		}
		w.Write([]byte("OK"))
	})
	return r
}

func TestCheckPolicy_NonRootNoPolicies_Denied(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return nil, nil // no policies
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 403, got %d, body: %s", resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "AccessDenied") {
		t.Errorf("Expected AccessDenied error, got: %s", string(body))
	}
}

func TestCheckPolicy_NonRootWithAllow_Passes(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return []iamdomain.PolicyDocument{
				{
					Version: "2012-10-17",
					Statement: []iamdomain.Statement{
						{
							Effect:   "Allow",
							Action:   iamdomain.StringOrArr{"ec2:RunInstances"},
							Resource: iamdomain.StringOrArr{"*"},
						},
					},
				},
			}, nil
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestCheckPolicy_NonRootWithExplicitDeny_Denied(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return []iamdomain.PolicyDocument{
				{
					Version: "2012-10-17",
					Statement: []iamdomain.Statement{
						{
							Effect:   "Allow",
							Action:   iamdomain.StringOrArr{"ec2:*"},
							Resource: iamdomain.StringOrArr{"*"},
						},
						{
							Effect:   "Deny",
							Action:   iamdomain.StringOrArr{"ec2:RunInstances"},
							Resource: iamdomain.StringOrArr{"*"},
						},
					},
				},
			}, nil
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 403, got %d, body: %s", resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "AccessDenied") {
		t.Errorf("Expected AccessDenied error, got: %s", string(body))
	}
}

func TestCheckPolicy_RootGlobalAccount_Bypasses(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "root",
					AccountID:       awsidentifiers.GlobalAccountID,
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			// Return explicit deny — root should bypass anyway
			return []iamdomain.PolicyDocument{
				{
					Version: "2012-10-17",
					Statement: []iamdomain.Statement{
						{
							Effect:   "Deny",
							Action:   iamdomain.StringOrArr{"*"},
							Resource: iamdomain.StringOrArr{"*"},
						},
					},
				},
			}, nil
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200 (root bypass), got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestCheckPolicy_MissingAccountID_InternalError(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	// AccessKey with empty AccountID — checkPolicy should return InternalError
	mockSvc := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "", // empty
					Status:          "Active",
				},
			},
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 500, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestCheckPolicy_GetUserPoliciesError_InternalError(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return nil, errors.New("nats: timeout")
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 500, got %d, body: %s", resp.StatusCode, string(body))
	}
}

// TestSigV4Auth_NATSDisconnectedShortCircuit: with nil IAMService a post-lookup
// failure would be 500; a 503 here proves the NATS short-circuit fired first.
func TestSigV4Auth_NATSDisconnectedShortCircuit(t *testing.T) {
	ns, _ := testutil.StartTestNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	nc.Close() // disconnected — IsConnected() returns false

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		NATSConn:       nc,
		IAMService:     nil,
	})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected 503, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	if !strings.Contains(xmlStr, "<Code>ServiceUnavailable</Code>") {
		t.Errorf("Expected ServiceUnavailable code in body, got: %s", xmlStr)
	}
	if !strings.Contains(xmlStr, "cluster unavailable: NATS disconnected") {
		t.Errorf("Expected cluster-unavailable message in body, got: %s", xmlStr)
	}
	if !strings.Contains(xmlStr, "/local/status") {
		t.Errorf("Expected /local/status hint in body, got: %s", xmlStr)
	}
}

// --- Session credential (ASIA) auth tests ---

// setupSessionTestApp wires a gateway with IAM+STS mocks. The stored secret is
// real AES-GCM so the SigV4 verify path exercises the actual decrypt code.
func setupSessionTestApp(t *testing.T, expiresAt time.Time) (http.Handler, *gwfix.STS) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)

	cred := &stsdomain.SessionCredential{
		AccessKeyID:       gwfix.SessionAKID,
		SecretEncrypted:   encryptedSecret,
		SessionTokenHMAC:  "ignored-in-mock", // mock verifies by plaintext map
		AccountID:         "123456789012",
		PrincipalType:     gwfix.PrincipalTypeAssumedRole,
		AssumedRoleARN:    gwfix.AssumedARN,
		UnderlyingRoleARN: gwfix.RoleARN,
		RoleID:            "AROAEXAMPLEAAAAAA",
		AssumedRoleID:     "AROAEXAMPLEAAAAAA:test-session",
		SessionName:       "test-session",
		ExpiresAt:         expiresAt,
		CreatedAt:         time.Now().UTC().Add(-time.Minute),
	}
	return mountSessionGateway(t, cred)
}

// mountSessionGateway wires SigV4 middleware over a handler that echoes the
// principal context; varies cred to drive the principal-type branch in resolveSessionAKID.
func mountSessionGateway(t *testing.T, cred *stsdomain.SessionCredential) (http.Handler, *gwfix.STS) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)

	stsMock := &gwfix.STS{
		Sessions: map[string]*stsdomain.SessionCredential{cred.AccessKeyID: cred},
		Tokens:   map[string]string{cred.AccessKeyID: gwfix.SessionToken},
	}

	iamMock := &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{
			gwfix.AccessKey: {
				AccessKeyID:     gwfix.AccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "alice",
				AccountID:       "999999999999",
				Status:          "Active",
			},
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     iamMock,
		STSService:     stsMock,
	})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
		caller := callerOf(req)
		fmt.Fprintf(w,
			"identity=%v|accountId=%v|principalType=%v|assumedRoleARN=%v",
			caller.Identity,
			caller.AccountID,
			caller.PrincipalType,
			caller.AssumedRoleARN,
		)
	})
	return r, stsMock
}

func TestSigV4Auth_Session_ValidSignature(t *testing.T) {
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, gwfix.SessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "identity=test-session")
	assert.Contains(t, bodyStr, "accountId=123456789012")
	assert.Contains(t, bodyStr, "principalType=assumed-role")
	assert.Contains(t, bodyStr, "assumedRoleARN="+gwfix.AssumedARN)
}

func TestSigV4Auth_Session_UserPrincipal(t *testing.T) {
	// GetSessionToken session (PrincipalType "user") must resolve back to the IAM
	// user with no assumed-role fields, so buildCallerARN yields arn:aws:iam::A:user/N.
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	cred := &stsdomain.SessionCredential{
		AccessKeyID:      gwfix.SessionAKID,
		SecretEncrypted:  encryptedSecret,
		SessionTokenHMAC: "ignored-in-mock",
		AccountID:        "123456789012",
		PrincipalType:    gwfix.PrincipalTypeUser,
		SessionName:      "alice",
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
		CreatedAt:        time.Now().UTC().Add(-time.Minute),
	}
	handler, _ := mountSessionGateway(t, cred)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, gwfix.SessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "identity=alice")
	assert.Contains(t, bodyStr, "accountId=123456789012")
	assert.Contains(t, bodyStr, "principalType=user")
	// A user session carries no assumed-role ARN, so the context value is unset.
	assert.True(t, strings.HasSuffix(bodyStr, "assumedRoleARN="), "body: %s", bodyStr)
}

func TestSigV4Auth_Session_EmptyPrincipalType_ResolvesAssumedRole(t *testing.T) {
	// Pre-PrincipalType records have an empty value and must still resolve as assumed-role.
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	cred := &stsdomain.SessionCredential{
		AccessKeyID:       gwfix.SessionAKID,
		SecretEncrypted:   encryptedSecret,
		SessionTokenHMAC:  "ignored-in-mock",
		AccountID:         "123456789012",
		PrincipalType:     "", // pre-migration record
		AssumedRoleARN:    gwfix.AssumedARN,
		UnderlyingRoleARN: gwfix.RoleARN,
		RoleID:            "AROAEXAMPLEAAAAAA",
		AssumedRoleID:     "AROAEXAMPLEAAAAAA:test-session",
		SessionName:       "test-session",
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		CreatedAt:         time.Now().UTC().Add(-time.Minute),
	}
	handler, _ := mountSessionGateway(t, cred)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, gwfix.SessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "identity=test-session")
	assert.Contains(t, bodyStr, "principalType=assumed-role")
	assert.Contains(t, bodyStr, "assumedRoleARN="+gwfix.AssumedARN)
}

// A fault is not a verdict: an IAM outage must surface as InternalError so the
// client retries rather than discarding a credential that is still good.
func TestSigV4Auth_Session_PrincipalCheckFault_IsInternalError(t *testing.T) {
	handler, stsMock := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))
	stsMock.PrincipalErr = errors.New("jetstream unavailable")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, gwfix.SessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(body), awserrors.ErrorInternalError)
}

// The other half of the ordering pin: a good signature does reach the check.
func TestSigV4Auth_Session_ValidSignature_RunsPrincipalCheck(t *testing.T) {
	handler, stsMock := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, gwfix.SessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	assert.Equal(t, int32(1), stsMock.PrincipalChecks.Load())
}

// A long-lived credential has no session record to check, so the door skips it.
func TestSigV4Auth_LongLived_SkipsPrincipalCheck(t *testing.T) {
	handler, stsMock := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	doRequest(handler, req)
	assert.Zero(t, stsMock.PrincipalChecks.Load())
}

func TestSigV4Auth_AKIA_PrincipalTypeUser(t *testing.T) {
	// Long-lived AKIA path must seal principalType=user for correct policy gating.
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)

	iamMock := &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{
			gwfix.AccessKey: {
				AccessKeyID:     gwfix.AccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "alice",
				AccountID:       "111111111111",
				Status:          "Active",
			},
		},
	}
	gw := withEC2RealAuth(&GatewayConfig{DisableLogging: true, Region: gwfix.Region, IAMService: iamMock})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
		caller := callerOf(req)
		fmt.Fprintf(w, "principalType=%v|assumedRoleARN=%v", caller.PrincipalType, caller.AssumedRoleARN)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)
	resp := doRequest(r, req)
	body, _ := io.ReadAll(resp.Body)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	assert.Contains(t, string(body), "principalType=user")
	// assumedRoleARN is empty for a long-lived user.
	assert.True(t, strings.HasSuffix(string(body), "assumedRoleARN="), "body: %s", string(body))
}

func TestSigV4Auth_Session_STSServiceNil(t *testing.T) {
	// Session AKID with nil STSService is a misconfiguration — must fail with InternalError.
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	iamMock := &gwfix.IAM{
		MasterKey:  gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{},
	}
	_ = encryptedSecret
	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     iamMock,
		STSService:     nil,
	})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, gwfix.SessionToken)

	resp := doRequest(r, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(body), "InternalError")
}

// TestCheckPolicy_SessionNameCollision is the privilege-escalation regression:
// session SessionName="alice" (colliding with an IAM user) + underlying role "some-role"
// that grants nothing. The enforcement path must resolve the underlying role —
// never the SessionName — so the request is denied and GetUserPolicies is never called.
func TestCheckPolicy_SessionNameCollision(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)

	var userPoliciesCalled bool
	var resolvedRoleName string

	// If SessionName were ever used as the policy-lookup key, the request would
	// inherit alice's ec2:* permissions.
	iamMock := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			userPoliciesCalled = true
			return gwfix.AllowPolicy("ec2:*"), nil
		},
		GetRolePoliciesFn: func(_, roleName string) ([]iamdomain.PolicyDocument, error) {
			resolvedRoleName = roleName
			return nil, nil
		},
	}

	cred := &stsdomain.SessionCredential{
		AccessKeyID:       gwfix.SessionAKID,
		SecretEncrypted:   encryptedSecret,
		AccountID:         "123456789012",
		PrincipalType:     gwfix.PrincipalTypeAssumedRole,
		AssumedRoleARN:    "arn:aws:sts::123456789012:assumed-role/some-role/alice",
		UnderlyingRoleARN: "arn:aws:iam::123456789012:role/some-role",
		SessionName:       "alice",
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		CreatedAt:         time.Now().UTC().Add(-time.Minute),
	}
	stsMock := &gwfix.STS{
		Sessions: map[string]*stsdomain.SessionCredential{gwfix.SessionAKID: cred},
		Tokens:   map[string]string{gwfix.SessionAKID: gwfix.SessionToken},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     iamMock,
		STSService:     stsMock,
	})

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, gwfix.SessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "AccessDenied")
	assert.Equal(t, "some-role", resolvedRoleName)
	assert.False(t, userPoliciesCalled, "priv-esc: assumed-role session must not consult GetUserPolicies")
}

// newAssumedRoleEnforcementGateway wires a gateway for assumed-role checkPolicy tests.
// The user resolver fails the test if called — an assumed-role must never be evaluated as a user.
func newAssumedRoleEnforcementGateway(t *testing.T, cred *stsdomain.SessionCredential,
	getRoleFn func(accountID, roleName string) ([]iamdomain.PolicyDocument, error),
) *GatewayConfig {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	cred.SecretEncrypted = encryptedSecret

	iamMock := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{MasterKey: gwfix.MasterKey},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			t.Errorf("priv-esc: assumed-role principal must not consult GetUserPolicies")
			return nil, nil
		},
		GetRolePoliciesFn: getRoleFn,
	}
	stsMock := &gwfix.STS{
		Sessions: map[string]*stsdomain.SessionCredential{cred.AccessKeyID: cred},
		Tokens:   map[string]string{cred.AccessKeyID: gwfix.SessionToken},
	}
	return withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     iamMock,
		STSService:     stsMock,
	})
}

// setupPolicyResourceTestHandler is setupPolicyTestHandler for resource-scoped
// checks (e.g. iam:PassRole on a specific role ARN).
func setupPolicyResourceTestHandler(gw *GatewayConfig, service, action, resource string) http.Handler {
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
		if err := gw.checkPolicyResources(req, service, action, []string{resource}); err != nil {
			gw.ErrorHandler(w, req, err)
			return
		}
		_, _ = w.Write([]byte("OK"))
	})
	return r
}

// doSessionPolicyRequest signs and runs a session-credential request, returning the response.
func doSessionPolicyRequest(t *testing.T, handler http.Handler, akid string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, akid, gwfix.SecretKey, gwfix.SessionToken)
	return doRequest(handler, req)
}

// TestCheckPolicy_AssumedRole_AllowedByRolePolicy: the underlying role's managed
// policy grants the action, so the session is allowed — the symmetric twin of
// predastore's assumed-role S3 enforcement.
func TestCheckPolicy_AssumedRole_AllowedByRolePolicy(t *testing.T) {
	cred := gwfix.AssumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, roleName string) ([]iamdomain.PolicyDocument, error) {
		require.Equal(t, "app-role", roleName)
		return gwfix.AllowPolicy("ec2:RunInstances"), nil
	})

	resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
}

// TestCheckPolicy_AssumedRole_ImplicitDeny: the role policy matches neither the
// action nor an explicit Deny, so the default deny applies.
func TestCheckPolicy_AssumedRole_ImplicitDeny(t *testing.T) {
	cred := gwfix.AssumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return gwfix.AllowPolicy("ec2:DescribeInstances"), nil // does not cover RunInstances
	})

	resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "AccessDenied")
}

// TestCheckPolicy_AssumedRole_ExplicitDeny: an explicit Deny in the role policy
// overrides any Allow.
func TestCheckPolicy_AssumedRole_ExplicitDeny(t *testing.T) {
	cred := gwfix.AssumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return []iamdomain.PolicyDocument{{
			Version: "2012-10-17",
			Statement: []iamdomain.Statement{
				{Effect: "Allow", Action: iamdomain.StringOrArr{"ec2:*"}, Resource: iamdomain.StringOrArr{"*"}},
				{Effect: "Deny", Action: iamdomain.StringOrArr{"ec2:RunInstances"}, Resource: iamdomain.StringOrArr{"*"}},
			},
		}}, nil
	})

	resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "AccessDenied")
}

// TestCheckPolicy_AssumedRole_ZeroPolicyRole_DenyAll: a role that resolves but
// has no attached policies can do nothing — assumability does not imply
// permissions.
func TestCheckPolicy_AssumedRole_ZeroPolicyRole_DenyAll(t *testing.T) {
	cred := gwfix.AssumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return nil, nil // role exists, zero attached policies
	})

	resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "AccessDenied")
}

// TestCheckPolicy_AssumedRole_SessionNamedRoot_NoBypass: a session whose
// attacker-chosen SessionName is "root" in the global account must NOT hit the
// user-branch root short-circuit; it is evaluated against its role like any
// other session.
func TestCheckPolicy_AssumedRole_SessionNamedRoot_NoBypass(t *testing.T) {
	cred := gwfix.AssumedRoleSessionCred("root",
		"arn:aws:iam::"+awsidentifiers.GlobalAccountID+":role/sneaky-role", awsidentifiers.GlobalAccountID)
	var roleResolved bool
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, roleName string) ([]iamdomain.PolicyDocument, error) {
		roleResolved = true
		require.Equal(t, "sneaky-role", roleName)
		return nil, nil // role grants nothing
	})

	resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "AccessDenied")
	assert.True(t, roleResolved, "root-named session must be evaluated against its role, not bypassed")
}

// TestCheckPolicy_AssumedRole_EdgeARNs_Denied: a missing/legacy, cross-account,
// or malformed underlying-role ARN fails closed with AccessDenied (403, not a
// 500), and never reaches the role resolver.
func TestCheckPolicy_AssumedRole_EdgeARNs_Denied(t *testing.T) {
	cases := []struct {
		name              string
		underlyingRoleARN string
	}{
		{"empty/legacy", ""},
		{"cross-account", "arn:aws:iam::999999999999:role/other"},
		{"malformed", "not-an-arn"},
		{"non-role-resource", "arn:aws:iam::123456789012:user/alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred := gwfix.AssumedRoleSessionCred("app", tc.underlyingRoleARN, "123456789012")
			gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]iamdomain.PolicyDocument, error) {
				t.Errorf("role resolver must not be consulted for an edge-case ARN (%s)", tc.name)
				return nil, nil
			})

			resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
			body, _ := io.ReadAll(resp.Body)
			assert.Equal(t, http.StatusForbidden, resp.StatusCode, "must deny, not 500")
			assert.Contains(t, string(body), "AccessDenied")
		})
	}
}

// TestCheckPolicy_AssumedRole_ResolveError_InternalError: a non-transient role
// resolve error fails closed — never an allow-on-error.
func TestCheckPolicy_AssumedRole_ResolveError_InternalError(t *testing.T) {
	cred := gwfix.AssumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity) // role deleted after mint
	})

	resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(body), "InternalError")
}

// TestCheckPolicy_AssumedRole_TransientNATS_RetriesThenFails: a transient NATS
// error is retried 3× then fails closed (no allow-on-error).
func TestCheckPolicy_AssumedRole_TransientNATS_RetriesThenFails(t *testing.T) {
	shortenPolicyResolveRetry(t)
	cred := gwfix.AssumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	var calls int
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		calls++
		return nil, nats.ErrNoResponders
	})

	resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(body), "InternalError")
	assert.Equal(t, 3, calls, "transient NATS error should retry 3×")
}

// TestCheckPolicy_AssumedRole_PassRole_ScopedToTargetARN_Allowed locks the
// iam:PassRole resource match: the role's policy scopes PassRole to the exact
// target role ARN, so passing that role is allowed.
func TestCheckPolicy_AssumedRole_PassRole_ScopedToTargetARN_Allowed(t *testing.T) {
	const targetRoleARN = "arn:aws:iam::123456789012:role/instance-role"
	cred := gwfix.AssumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/launcher-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return gwfix.AllowPolicyResource("iam:PassRole", targetRoleARN), nil
	})

	handler := setupPolicyResourceTestHandler(gw, "iam", "PassRole", targetRoleARN)
	resp := doSessionPolicyRequest(t, handler, cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
}

// TestCheckPolicy_AssumedRole_PassRole_ScopedToOtherARN_Denied: PassRole scoped
// to a different role ARN must not authorise passing the target role.
func TestCheckPolicy_AssumedRole_PassRole_ScopedToOtherARN_Denied(t *testing.T) {
	const targetRoleARN = "arn:aws:iam::123456789012:role/instance-role"
	cred := gwfix.AssumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/launcher-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return gwfix.AllowPolicyResource("iam:PassRole", "arn:aws:iam::123456789012:role/some-other-role"), nil
	})

	handler := setupPolicyResourceTestHandler(gw, "iam", "PassRole", targetRoleARN)
	resp := doSessionPolicyRequest(t, handler, cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "AccessDenied")
}

// TestCheckPolicy_AssumedRole_PassRole_WildcardResource_Allowed: a policy
// granting iam:PassRole on "*" authorises passing any role ARN — the classic
// wildcard PassRole grant. Locks the wildcard branch of the resource match.
func TestCheckPolicy_AssumedRole_PassRole_WildcardResource_Allowed(t *testing.T) {
	const targetRoleARN = "arn:aws:iam::123456789012:role/instance-role"
	cred := gwfix.AssumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/launcher-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return gwfix.AllowPolicyResource("iam:PassRole", "*"), nil
	})

	handler := setupPolicyResourceTestHandler(gw, "iam", "PassRole", targetRoleARN)
	resp := doSessionPolicyRequest(t, handler, cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
}

func TestMismatchAction_RecoversActionAndRewindsBody(t *testing.T) {
	body := []byte("Action=ImportKeyPair&Version=2016-11-15&KeyName=demo&PublicKeyMaterial=c3NoLXJzYQ%3D%3D")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	assert.Equal(t, "ImportKeyPair", mismatchAction(req, "ec2"))

	// The error path still writes a response, so the body must survive the peek.
	rest, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, body, rest)
}

func TestMismatchAction_SkipsS3AndPrefersQueryString(t *testing.T) {
	// An S3 body is object data, so it must never be parsed as query args.
	s3req := httptest.NewRequest(http.MethodPut, "/bucket/key", bytes.NewReader([]byte("Action=NotReally")))
	assert.Empty(t, mismatchAction(s3req, "s3"))

	qreq := httptest.NewRequest(http.MethodGet, "/?Action=DescribeInstances", nil)
	assert.Equal(t, "DescribeInstances", mismatchAction(qreq, "ec2"))
}

func TestRedactedCanonicalRequest_MasksSessionToken(t *testing.T) {
	const token = "SUPERSECRETSESSIONTOKEN"
	body := []byte("Action=DescribeInstances")
	req := httptest.NewRequest(http.MethodPost, "http://gw.local/", bytes.NewReader(body))
	req.Header.Set("X-Amz-Security-Token", token)
	gwfix.SignRequest(t, req, body, gwfix.AccessKey, gwfix.SecretKey)

	sig, err := sigv4.Parse(req)
	require.NoError(t, err)

	out := sig.RedactedCanonicalRequest()
	assert.NotContains(t, out, token, "session token must not reach the log")
	assert.Contains(t, out, "x-amz-security-token:<redacted>")
	// The rest must stay intact, or the log is useless for diffing.
	assert.Contains(t, out, "POST")
	assert.Contains(t, out, "x-amz-date:")
}

// --- Unserved-service rejection: envelope and message (A + B) ---

// A scope the gateway does not serve is rejected at the supportedServices
// gate before any signature check, so the response must not blame the
// credentials, must carry InvalidAction (the same code an unimplemented
// action on a served service already returns) rather than a signature
// failure that never happened, and must render in the generic REST-XML
// envelope rather than the EC2 query shape a non-query client cannot
// deserialize.
func TestSigV4Auth_UnservedServiceNamesServiceNotCredentials(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequestAs(t, req, nil, gwfix.AccessKey, gwfix.SecretKey, "sqs", time.Now().UTC())

	resp := doRequest(handler, req)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var parsed struct {
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
		RequestID string `xml:"RequestId"`
	}
	require.NoError(t, xml.Unmarshal(body, &parsed), "body: %s", body)

	assert.Equal(t, "ErrorResponse", xmlRootName(t, body))
	assert.Equal(t, awserrors.ErrorInvalidAction, parsed.Error.Code)
	assert.Contains(t, parsed.Error.Message, "sqs")
	assert.NotContains(t, parsed.Error.Message, "credentials",
		"nothing was wrong with the credentials, which were never checked")
}

// An unserved scope whose request marks it as an AWS JSON-1.x client (sqs,
// sns, dynamodb, …) must get the JSON envelope: jsonErrorService has no entry
// for a scope the gateway does not serve, so without the request-derived
// fallback this always fell through to XML the client's SDK cannot parse.
func TestSigV4Auth_UnservedServiceJSONClientGetsJSONEnvelope(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Host = "localhost:9999"
	req.Header.Set("X-Amz-Target", "AmazonSQS.ListQueues")
	gwfix.SignRequestAs(t, req, nil, gwfix.AccessKey, gwfix.SecretKey, "sqs", time.Now().UTC())

	resp := doRequest(handler, req)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "InvalidActionException", resp.Header.Get("X-Amzn-Errortype"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "sqs")
	assert.NotContains(t, string(body), "<?xml")
}

// The same, driven by Content-Type rather than X-Amz-Target: some JSON
// clients (dynamodb) identify themselves this way instead.
func TestSigV4Auth_UnservedServiceJSONContentTypeGetsJSONEnvelope(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	gwfix.SignRequestAs(t, req, nil, gwfix.AccessKey, gwfix.SecretKey, "dynamodb", time.Now().UTC())

	resp := doRequest(handler, req)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "InvalidActionException", resp.Header.Get("X-Amzn-Errortype"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "dynamodb")
}

// An unserved scope whose request carries neither tell (a REST-XML client
// such as cloudfront's) must keep the XML envelope, which is correct for it.
func TestSigV4Auth_UnservedServiceNoJSONTellGetsXMLEnvelope(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequestAs(t, req, nil, gwfix.AccessKey, gwfix.SecretKey, "cloudfront", time.Now().UTC())

	resp := doRequest(handler, req)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "ErrorResponse", xmlRootName(t, body))
	assert.Contains(t, string(body), "cloudfront")
}

// --- ctxAction resolution for non-query-protocol services (C) ---

// JSON-1.1 and path-routed REST-JSON requests carry no query-protocol Action,
// so before the fix ctxAction stayed empty and every such request shared the
// throttle bucket "<account>:unknown", which no per-action limit can match.
func TestSigV4Auth_ResolvesCtxActionForNonQueryServices(t *testing.T) {
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)

	mockSvc := &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{
			gwfix.AccessKey: {
				AccessKeyID:     gwfix.AccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "alice",
				AccountID:       "123456789012",
				Status:          "Active",
			},
		},
	}
	gw := withEC2RealAuth(&GatewayConfig{DisableLogging: true, Region: gwfix.Region, IAMService: mockSvc})
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(awsapi.NewRegistration(awsapi.Deps{})))
	require.NoError(t, b.Register(acmawsapi.NewRegistration(acmawsapi.Deps{})))
	require.NoError(t, b.Register(taggingawsapi.NewRegistration(taggingawsapi.Deps{})))
	require.NoError(t, b.Register(ecsawsapi.NewRegistration(ecsawsapi.Deps{})))
	require.NoError(t, b.Register(eksawsapi.NewRegistration(eksawsapi.Deps{})))
	gw.Services = b.Build()

	var gotAction, gotThrottleKey string
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		gotAction = reqctx.Action(r.Context())
		gotThrottleKey, _ = gw.throttleKeyFuncs()[1](r)
		w.Write([]byte("OK"))
	})

	testCases := []struct {
		name       string
		method     string
		path       string
		service    string
		target     string
		wantAction string
	}{
		{"JSON-1.1 X-Amz-Target", http.MethodPost, "/", "tagging",
			"ResourceGroupsTaggingAPI_20170126.GetResources", "GetResources"},
		{"path-routed EKS", http.MethodGet, "/clusters", "eks", "", "ListClusters"},
		{"JSON-1.1 X-Amz-Target ECR", http.MethodPost, "/", "ecr",
			awsapi.TargetPrefix + ".ListRepositories", "ListRepositories"},
		{"JSON-1.1 X-Amz-Target ACM", http.MethodPost, "/", "acm",
			"CertificateManager.ListCertificates", "ListCertificates"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotAction, gotThrottleKey = "", ""
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Host = "localhost:9999"
			if tc.target != "" {
				req.Header.Set("X-Amz-Target", tc.target)
			}
			gwfix.SignRequestAs(t, req, nil, gwfix.AccessKey, gwfix.SecretKey, tc.service, time.Now().UTC())

			resp := doRequest(r, req)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, tc.wantAction, gotAction)
			assert.NotEqual(t, "unknown", gotThrottleKey,
				"throttle bucket must not fall back to the shared unknown key")
			assert.Equal(t, tc.wantAction, gotThrottleKey)
		})
	}
}

// Route 53 is global: the AWS CLI and SDKs sign it for us-east-1 against the
// default endpoint and for the configured region against an override. Every
// other service stays pinned to the gateway's region.
func TestExpectedSigningRegion(t *testing.T) {
	gw := withServices(&GatewayConfig{Region: "ap-southeast-2"}, route53awsapi.NewRegistration(route53awsapi.Deps{}))
	for _, tc := range []struct {
		service, claimed, want string
	}{
		{"route53", "us-east-1", "us-east-1"},
		{"route53", "ap-southeast-2", "ap-southeast-2"},
		{"route53", "eu-west-1", "ap-southeast-2"},
		{"ec2", "us-east-1", "ap-southeast-2"},
	} {
		assert.Equalf(t, tc.want, gw.expectedSigningRegion(tc.service, tc.claimed), "%s signed for %s", tc.service, tc.claimed)
	}
}

// The check runs past sig.Verify, so everything reaching it holds a real
// secret. Gone and Replaced still count toward the lockout: an attacker
// working through a dump of leaked session credentials looking for one whose
// principal survives presents a distinct AKID each time. Legacy must not, and
// that is not symmetry for its own sake — every record predating the field
// fails on the deploy that ships this check, so a shared egress address would
// cross MaxFailures on distinct-attempt volume alone and be told to retry
// later by a fleet that has nothing to retry with.
func TestSigV4Auth_SessionPrincipalVerdict_RateLimitRecording(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		recorded bool
	}{
		{"gone", stsdomain.ErrSessionPrincipalGone, true},
		{"replaced", stsdomain.ErrSessionPrincipalReplaced, true},
		{"legacy", stsdomain.ErrSessionPrincipalLegacy, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rl := authlimit.NewAuthRateLimiter()
			defer rl.Stop()
			gw := &GatewayConfig{DisableLogging: true, RateLimiter: rl}

			// The real verifier decides whether a verdict counts; the gateway records it.
			encrypted, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
			require.NoError(t, err)
			cred := &stsdomain.SessionCredential{
				AccessKeyID: gwfix.SessionAKID, SecretEncrypted: encrypted, AccountID: "123456789012",
				PrincipalType: gwfix.PrincipalTypeAssumedRole, SessionName: "test-session",
				ExpiresAt: time.Now().UTC().Add(time.Hour),
			}
			sts := &gwfix.STS{
				Sessions:     map[string]*stsdomain.SessionCredential{gwfix.SessionAKID: cred},
				Tokens:       map[string]string{gwfix.SessionAKID: gwfix.SessionToken},
				PrincipalErr: tc.err,
			}
			v := credverify.New(&gwfix.IAM{MasterKey: gwfix.MasterKey}, sts)
			req := credential.Request{AccessKeyID: gwfix.SessionAKID, SecurityToken: gwfix.SessionToken}
			authed, f := v.Authenticate(req, func(string) error { return nil })
			require.Nil(t, f)
			_, f = v.Resolve(req, authed)
			require.NotNil(t, f)
			require.Equal(t, awserrors.ErrorInvalidClientTokenId, f.Code)

			const ip = "10.15.8.11"
			gw.recordCredentialFailure(authSource{limitKey: ip, logIP: ip}, f)

			// Read at the lockout boundary: with exactly `recorded` failures from
			// the verdict, n more distinct ones leave the address open and one
			// further locks it. Zero or two recorded would miss either edge.
			recorded := 0
			if tc.recorded {
				recorded = 1
			}
			n := authlimit.MaxFailures - 1 - recorded
			recordProbeFailures(rl, ip, "below", n)
			assert.Empty(t, rl.CheckIP(ip), "address must stay open below the threshold")
			recordProbeFailures(rl, ip, "edge", 1)
			assert.Equal(t, awserrors.ErrorRequestLimitExceeded, rl.CheckIP(ip),
				"the next distinct failure must lock the address")
		})
	}
}

// Every scope jsonErrorService already lists must keep picking the JSON
// envelope by exactly the path it did before the fallback was added — a
// regression guard proving the fallback widened nothing for served scopes.
func TestWriteSigV4Error_JSONErrorServicesUnaffectedByFallback(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(awsapi.NewRegistration(awsapi.Deps{})))
	require.NoError(t, b.Register(acmawsapi.NewRegistration(acmawsapi.Deps{})))
	require.NoError(t, b.Register(taggingawsapi.NewRegistration(taggingawsapi.Deps{})))
	require.NoError(t, b.Register(ecsawsapi.NewRegistration(ecsawsapi.Deps{})))
	require.NoError(t, b.Register(eksawsapi.NewRegistration(eksawsapi.Deps{})))
	require.NoError(t, b.Register(ochreawsapi.NewRegistration(ochreawsapi.Deps{})))
	gw.Services = b.Build()

	for _, svc := range []string{"eks", "ecr", "acm", "ecs", "tagging",
		"bedrock", "bedrock-runtime", "bedrock-agent", "bedrock-agent-runtime"} {
		t.Run(svc, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req = req.WithContext(reqctx.WithService(req.Context(), svc))
			w := httptest.NewRecorder()

			gw.writeSigV4Error(w, req, awserrors.ErrorSignatureDoesNotMatch, "")

			resp := w.Result()
			assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
			assert.Equal(t, "SignatureDoesNotMatchException", resp.Header.Get("X-Amzn-Errortype"))
		})
	}
}

func TestCheckPolicy_RootNonGlobalAccount_StillEvaluated(t *testing.T) {
	// A user named "root" but on a non-global account should still have
	// policies evaluated (only root + GlobalAccountID bypasses).
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "root",
					AccountID:       "999999999999", // not GlobalAccountID
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return nil, nil // no policies → should deny
		},
	}

	gw := withEC2RealAuth(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		IAMService:     mockSvc,
	})

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)

	// root on non-global account is evaluated by the policy engine like any
	// other user. With no policies attached, the default deny applies.
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 403, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestSigV4Auth_Session_AKIDNotInBucket(t *testing.T) {
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, "ASIAOTHERAKIDAAAAAAA", gwfix.SecretKey, gwfix.SessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "InvalidClientTokenId")
}

func TestSigV4Auth_Session_Expired(t *testing.T) {
	// Past expiry but within janitor grace window: must reject as ExpiredToken.
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(-time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, gwfix.SessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(body), "ExpiredToken")
}

func TestSigV4Auth_Session_MissingSecurityToken(t *testing.T) {
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	// No X-Amz-Security-Token — ASIA path requires it.
	gwfix.SignRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "InvalidClientTokenId")
}

// A session whose backing principal was deleted, or recreated under the same
// name with a different immutable ID, must not authenticate. InvalidClientTokenId
// rather than ExpiredToken: the credential itself has not expired.
func TestSigV4Auth_Session_StalePrincipalRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"gone", stsdomain.ErrSessionPrincipalGone},
		{"replaced", stsdomain.ErrSessionPrincipalReplaced},
		{"legacy", stsdomain.ErrSessionPrincipalLegacy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, stsMock := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))
			stsMock.PrincipalErr = tc.err

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = "localhost:9999"
			gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, gwfix.SessionToken)

			resp := doRequest(handler, req)
			body, _ := io.ReadAll(resp.Body)

			assert.Equal(t, http.StatusForbidden, resp.StatusCode)
			assert.Contains(t, string(body), awserrors.ErrorInvalidClientTokenId)
		})
	}
}

// Ordering pin: the continuity check reads the IAM users and roles buckets, and
// an AKID travels in the Authorization header in cleartext. It must stay behind
// sig.Verify, so a bad signature is answered without the check running at all.
func TestSigV4Auth_Session_StalePrincipal_BadSignatureWinsFirst(t *testing.T) {
	handler, stsMock := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))
	stsMock.PrincipalErr = stsdomain.ErrSessionPrincipalReplaced

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, "wrong-secret-key", gwfix.SessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Contains(t, string(body), awserrors.ErrorSignatureDoesNotMatch)
	assert.Zero(t, stsMock.PrincipalChecks.Load(),
		"continuity check must not run before the signature verifies")
}

func TestSigV4Auth_Session_WrongSecurityToken(t *testing.T) {
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignSessionRequest(t, req, nil, gwfix.SessionAKID, gwfix.SecretKey, "wrong-wire-token")

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "InvalidClientTokenId")
}

func TestSigV4Auth_UnknownAKIDPrefix_NoLookup(t *testing.T) {
	handler, stsMock := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	// Neither AKIA nor ASIA — must reject without any IAM/STS lookup.
	gwfix.SignRequest(t, req, nil, "ZZIATESTAKIDXXXXXXXX", gwfix.SecretKey)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "InvalidClientTokenId")
	assert.Equal(t, int32(0), stsMock.Lookups.Load(), "STSService must not be consulted for unknown-prefix AKIDs")
}

func TestWriteSigV4Error_ResponseFormat(t *testing.T) {
	gw := withEC2RealAuth(&GatewayConfig{DisableLogging: true, Region: gwfix.Region, IAMService: allowAllIAMService()})

	testCases := []struct {
		errorCode      string
		expectedStatus int
	}{
		{awserrors.ErrorMissingAuthenticationToken, 403},
		{awserrors.ErrorIncompleteSignature, 400},
		{awserrors.ErrorInvalidClientTokenId, 403},
		{awserrors.ErrorSignatureDoesNotMatch, 403},
		{awserrors.ErrorInternalError, 500},
	}

	for _, tc := range testCases {
		t.Run(tc.errorCode, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)

			gw.writeSigV4Error(w, req, tc.errorCode, "")
			resp := w.Result()

			// Correct HTTP status code
			if resp.StatusCode != tc.expectedStatus {
				t.Errorf("Expected status %d for %s, got %d", tc.expectedStatus, tc.errorCode, resp.StatusCode)
			}

			// Content-Type is application/xml
			ct := resp.Header.Get("Content-Type")
			if ct != "application/xml" {
				t.Errorf("Expected Content-Type application/xml, got %q", ct)
			}

			// No ctxService is set here, so the signature never parsed and the
			// EC2 query envelope still applies, RequestID casing included.
			body, _ := io.ReadAll(resp.Body)
			bodyStr := string(body)

			if !strings.Contains(bodyStr, "<Code>"+tc.errorCode+"</Code>") {
				t.Errorf("Response body missing error code %q: %s", tc.errorCode, bodyStr)
			}
			if !strings.Contains(bodyStr, "<RequestID>") {
				t.Errorf("Response body missing RequestID: %s", bodyStr)
			}
			if !strings.Contains(bodyStr, "<?xml") {
				t.Errorf("Response body missing XML declaration: %s", bodyStr)
			}
		})
	}
}

func TestWriteSigV4Error_IgnoresClientRequestID(t *testing.T) {
	gw := withEC2RealAuth(&GatewayConfig{DisableLogging: true, Region: gwfix.Region, IAMService: allowAllIAMService()})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Amz-Request-Id", "test-request-id-123")

	gw.writeSigV4Error(w, req, awserrors.ErrorIncompleteSignature, "")

	body, _ := io.ReadAll(w.Result().Body)
	if strings.Contains(string(body), "test-request-id-123") {
		t.Errorf("Expected server-generated request ID, but client ID was used: %s", string(body))
	}
	if !strings.Contains(string(body), "<RequestID>") {
		t.Errorf("Expected RequestID in response, got: %s", string(body))
	}
}
