package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccessKey = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testRegion    = "us-east-1"
	testService   = "ec2"
)

// mockIAMService implements handlers_iam.IAMService for auth tests.
type mockIAMService struct {
	accessKeys map[string]*handlers_iam.AccessKey
	masterKey  []byte
}

func (m *mockIAMService) LookupAccessKey(accessKeyID string) (*handlers_iam.AccessKey, error) {
	ak, ok := m.accessKeys[accessKeyID]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return ak, nil
}

func (m *mockIAMService) CreateUser(_ string, _ *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	return nil, nil
}
func (m *mockIAMService) GetUser(_ string, _ *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListUsers(_ string, _ *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	return nil, nil
}
func (m *mockIAMService) DeleteUser(_ string, _ *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	return nil, nil
}
func (m *mockIAMService) CreateAccessKey(_ string, _ *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListAccessKeys(_ string, _ *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	return nil, nil
}
func (m *mockIAMService) DeleteAccessKey(_ string, _ *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) UpdateAccessKey(_ string, _ *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) CreatePolicy(_ string, _ *iam.CreatePolicyInput) (*iam.CreatePolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) GetPolicy(_ string, _ *iam.GetPolicyInput) (*iam.GetPolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) GetPolicyVersion(_ string, _ *iam.GetPolicyVersionInput) (*iam.GetPolicyVersionOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListPolicyVersions(_ string, _ *iam.ListPolicyVersionsInput) (*iam.ListPolicyVersionsOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListPolicies(_ string, _ *iam.ListPoliciesInput) (*iam.ListPoliciesOutput, error) {
	return nil, nil
}
func (m *mockIAMService) DeletePolicy(_ string, _ *iam.DeletePolicyInput) (*iam.DeletePolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) AttachUserPolicy(_ string, _ *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) DetachUserPolicy(_ string, _ *iam.DetachUserPolicyInput) (*iam.DetachUserPolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListAttachedUserPolicies(_ string, _ *iam.ListAttachedUserPoliciesInput) (*iam.ListAttachedUserPoliciesOutput, error) {
	return nil, nil
}
func (m *mockIAMService) GetUserPolicies(_, _ string) ([]handlers_iam.PolicyDocument, error) {
	return nil, nil
}
func (m *mockIAMService) GetRolePolicies(_, _ string) ([]handlers_iam.PolicyDocument, error) {
	return nil, nil
}
func (m *mockIAMService) DecryptSecret(ciphertext string) (string, error) {
	return handlers_iam.DecryptSecret(ciphertext, m.masterKey)
}
func (m *mockIAMService) SeedBootstrap(_ *handlers_iam.BootstrapData) error { return nil }
func (m *mockIAMService) IsEmpty() (bool, error)                            { return true, nil }
func (m *mockIAMService) CreateAccount(_ string) (*handlers_iam.Account, error) {
	return nil, nil
}
func (m *mockIAMService) GetAccount(_ string) (*handlers_iam.Account, error) { return nil, nil }
func (m *mockIAMService) ListAccounts() ([]*handlers_iam.Account, error)     { return nil, nil }

func (m *mockIAMService) CreateRole(_ string, _ *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return nil, nil
}
func (m *mockIAMService) GetRole(_ string, _ *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListRoles(_ string, _ *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	return nil, nil
}
func (m *mockIAMService) DeleteRole(_ string, _ *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	return nil, nil
}
func (m *mockIAMService) UpdateRole(_ string, _ *iam.UpdateRoleInput) (*iam.UpdateRoleOutput, error) {
	return nil, nil
}
func (m *mockIAMService) UpdateAssumeRolePolicy(_ string, _ *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) AttachRolePolicy(_ string, _ *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) DetachRolePolicy(_ string, _ *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListAttachedRolePolicies(_ string, _ *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error) {
	return nil, nil
}
func (m *mockIAMService) PutRolePolicy(_ string, _ *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) GetRolePolicy(_ string, _ *iam.GetRolePolicyInput) (*iam.GetRolePolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) DeleteRolePolicy(_ string, _ *iam.DeleteRolePolicyInput) (*iam.DeleteRolePolicyOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListRolePolicies(_ string, _ *iam.ListRolePoliciesInput) (*iam.ListRolePoliciesOutput, error) {
	return nil, nil
}
func (m *mockIAMService) CreateInstanceProfile(_ string, _ *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	return nil, nil
}
func (m *mockIAMService) GetInstanceProfile(_ string, _ *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListInstanceProfiles(_ string, _ *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	return nil, nil
}
func (m *mockIAMService) DeleteInstanceProfile(_ string, _ *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListInstanceProfilesForRole(_ string, _ *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return nil, nil
}
func (m *mockIAMService) AddRoleToInstanceProfile(_ string, _ *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return nil, nil
}
func (m *mockIAMService) RemoveRoleFromInstanceProfile(_ string, _ *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ResolveInstanceProfile(_, _ string) (*handlers_iam.InstanceProfile, error) {
	return nil, nil
}
func (m *mockIAMService) CreateGroup(_ string, _ *iam.CreateGroupInput) (*iam.CreateGroupOutput, error) {
	return &iam.CreateGroupOutput{}, nil
}
func (m *mockIAMService) GetGroup(_ string, _ *iam.GetGroupInput) (*iam.GetGroupOutput, error) {
	return &iam.GetGroupOutput{}, nil
}
func (m *mockIAMService) ListGroups(_ string, _ *iam.ListGroupsInput) (*iam.ListGroupsOutput, error) {
	return &iam.ListGroupsOutput{}, nil
}
func (m *mockIAMService) DeleteGroup(_ string, _ *iam.DeleteGroupInput) (*iam.DeleteGroupOutput, error) {
	return &iam.DeleteGroupOutput{}, nil
}
func (m *mockIAMService) AddUserToGroup(_ string, _ *iam.AddUserToGroupInput) (*iam.AddUserToGroupOutput, error) {
	return &iam.AddUserToGroupOutput{}, nil
}
func (m *mockIAMService) RemoveUserFromGroup(_ string, _ *iam.RemoveUserFromGroupInput) (*iam.RemoveUserFromGroupOutput, error) {
	return &iam.RemoveUserFromGroupOutput{}, nil
}
func (m *mockIAMService) ListGroupsForUser(_ string, _ *iam.ListGroupsForUserInput) (*iam.ListGroupsForUserOutput, error) {
	return &iam.ListGroupsForUserOutput{}, nil
}
func (m *mockIAMService) AttachGroupPolicy(_ string, _ *iam.AttachGroupPolicyInput) (*iam.AttachGroupPolicyOutput, error) {
	return &iam.AttachGroupPolicyOutput{}, nil
}
func (m *mockIAMService) DetachGroupPolicy(_ string, _ *iam.DetachGroupPolicyInput) (*iam.DetachGroupPolicyOutput, error) {
	return &iam.DetachGroupPolicyOutput{}, nil
}
func (m *mockIAMService) ListAttachedGroupPolicies(_ string, _ *iam.ListAttachedGroupPoliciesInput) (*iam.ListAttachedGroupPoliciesOutput, error) {
	return &iam.ListAttachedGroupPoliciesOutput{}, nil
}
func (m *mockIAMService) PutGroupPolicy(_ string, _ *iam.PutGroupPolicyInput) (*iam.PutGroupPolicyOutput, error) {
	return &iam.PutGroupPolicyOutput{}, nil
}
func (m *mockIAMService) GetGroupPolicy(_ string, _ *iam.GetGroupPolicyInput) (*iam.GetGroupPolicyOutput, error) {
	return &iam.GetGroupPolicyOutput{}, nil
}
func (m *mockIAMService) DeleteGroupPolicy(_ string, _ *iam.DeleteGroupPolicyInput) (*iam.DeleteGroupPolicyOutput, error) {
	return &iam.DeleteGroupPolicyOutput{}, nil
}
func (m *mockIAMService) ListGroupPolicies(_ string, _ *iam.ListGroupPoliciesInput) (*iam.ListGroupPoliciesOutput, error) {
	return &iam.ListGroupPoliciesOutput{}, nil
}
func (m *mockIAMService) PutUserPolicy(_ string, _ *iam.PutUserPolicyInput) (*iam.PutUserPolicyOutput, error) {
	return &iam.PutUserPolicyOutput{}, nil
}
func (m *mockIAMService) GetUserPolicy(_ string, _ *iam.GetUserPolicyInput) (*iam.GetUserPolicyOutput, error) {
	return &iam.GetUserPolicyOutput{}, nil
}
func (m *mockIAMService) DeleteUserPolicy(_ string, _ *iam.DeleteUserPolicyInput) (*iam.DeleteUserPolicyOutput, error) {
	return &iam.DeleteUserPolicyOutput{}, nil
}
func (m *mockIAMService) ListUserPolicies(_ string, _ *iam.ListUserPoliciesInput) (*iam.ListUserPoliciesOutput, error) {
	return &iam.ListUserPoliciesOutput{}, nil
}

// testMasterKey is a fixed 32-byte key for deterministic tests.
var testMasterKey []byte

func init() {
	var err error
	testMasterKey, err = handlers_iam.GenerateMasterKey()
	if err != nil {
		panic("failed to generate test master key: " + err.Error())
	}
}

// signTestRequest signs req in place using SigV4. body must match what the
// middleware will read from r.Body so the sha256 matches X-Amz-Content-Sha256.
func signTestRequest(t *testing.T, req *http.Request, body []byte, accessKey, secret string, optFns ...func(*auth.Options)) {
	t.Helper()
	sum := sha256.Sum256(body)
	require.NoError(t, auth.SignReq(req, accessKey, secret,
		hex.EncodeToString(sum[:]), testService, testRegion, optFns...))
}

func setupTestApp(accessKey, secretKey string) http.Handler {
	encryptedSecret, err := handlers_iam.EncryptSecret(secretKey, testMasterKey)
	if err != nil {
		panic("failed to encrypt test secret: " + err.Error())
	}

	mockSvc := &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			accessKey: {
				AccessKeyID:     accessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "root",
				Status:          "Active",
			},
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	return r
}

func TestSigV4Auth_NoAuthorizationHeader(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

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
	handler := setupTestApp(testAccessKey, testSecretKey)

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
	handler := setupTestApp(testAccessKey, testSecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, "INVALID_ACCESS_KEY", testSecretKey)

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
	handler := setupTestApp(testAccessKey, testSecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, "WRONG_SECRET_KEY")

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SignatureDoesNotMatch") {
		t.Errorf("Expected SignatureDoesNotMatch error, got: %s", string(body))
	}
}

func TestSigV4Auth_ValidSignature(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestSigV4Auth_ValidSignatureWithBody(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	body := []byte("Action=DescribeInstances&Version=2016-11-15")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signTestRequest(t, req, body, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(respBody))
	}
}

func TestSigV4Auth_ValidSignatureWithQueryString(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	queryString := "Action=DescribeInstances&Version=2016-11-15"
	req := httptest.NewRequest(http.MethodGet, "/?"+queryString, nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(respBody))
	}
}

func TestSigV4Auth_InactiveAccessKey(t *testing.T) {
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			testAccessKey: {
				AccessKeyID:     testAccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "root",
				Status:          "Inactive",
			},
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403 for inactive key, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "InvalidClientTokenId") {
		t.Errorf("Expected InvalidClientTokenId error, got: %s", string(body))
	}
}

func TestSigV4Auth_NilIAMService(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     nil,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

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

func TestSigV4Auth_DecryptFailure(t *testing.T) {
	// Secret encrypted under a rotated master key — unverifiable credential must
	// return 403 InvalidClientTokenId, not 500, so the client re-authenticates.
	otherKey, err := handlers_iam.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey() error: %v", err)
	}
	encryptedWithOther, err := handlers_iam.EncryptSecret(testSecretKey, otherKey)
	if err != nil {
		t.Fatalf("EncryptSecret() error: %v", err)
	}

	mockSvc := &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			testAccessKey: {
				AccessKeyID:     testAccessKey,
				SecretAccessKey: encryptedWithOther, // encrypted with wrong key
				UserName:        "root",
				Status:          "Active",
			},
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403 for decrypt failure, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), awserrors.ErrorInvalidClientTokenId) {
		t.Errorf("Expected InvalidClientTokenId, got: %s", string(body))
	}
}

func TestSigV4Auth_RequestBodyTooLarge(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	oversizedBody := []byte(strings.Repeat("x", maxBodySize+1))
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversizedBody))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signTestRequest(t, req, oversizedBody, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("Expected status 413, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "RequestEntityTooLarge") {
		t.Errorf("Expected RequestEntityTooLarge error, got: %s", string(body))
	}
}

// --- Clock Skew / Replay Protection Tests ---

func TestSigV4Auth_ExpiredTimestamp(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	past := time.Now().UTC().Add(-6 * time.Minute) // exceeds 5-minute maxClockSkew
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey, auth.WithTime(past))

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
	handler := setupTestApp(testAccessKey, testSecretKey)

	future := time.Now().UTC().Add(6 * time.Minute) // exceeds 5-minute maxClockSkew
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey, auth.WithTime(future))

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "SignatureDoesNotMatch") {
		t.Errorf("Expected SignatureDoesNotMatch error, got: %s", string(body))
	}
}

func TestSigV4Auth_TimestampWithinSkew(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	recent := time.Now().UTC().Add(-4 * time.Minute) // within the 5-minute window
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey, auth.WithTime(recent))

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
	}
}

// --- Clock Skew Boundary Tests ---

func TestSigV4Auth_ClockSkewBoundary(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	testCases := []struct {
		name       string
		offset     time.Duration
		expectPass bool
	}{
		// 4m59s not 5m to absorb test execution time.
		{"just within 5 min past", -(4*time.Minute + 59*time.Second), true},
		{"just beyond 5 min past", -(5*time.Minute + 1*time.Second), false},
		{"just within 5 min future", 4*time.Minute + 59*time.Second, true},
		{"just beyond 5 min future", 5*time.Minute + 1*time.Second, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().UTC().Add(tc.offset)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = "localhost:9999"
			signTestRequest(t, req, nil, testAccessKey, testSecretKey, auth.WithTime(at))

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

func TestSigV4Auth_MissingXAmzDate(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)
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
	handler := setupTestApp(testAccessKey, testSecretKey)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)
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

// --- writeSigV4Error Response Format Tests ---

func TestWriteSigV4Error_ResponseFormat(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, Region: testRegion}

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

			gw.writeSigV4Error(w, req, tc.errorCode)
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

			// Body is well-formed XML containing the error code and RequestID
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
	gw := &GatewayConfig{DisableLogging: true, Region: testRegion}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Amz-Request-Id", "test-request-id-123")

	gw.writeSigV4Error(w, req, awserrors.ErrorIncompleteSignature)

	body, _ := io.ReadAll(w.Result().Body)
	if strings.Contains(string(body), "test-request-id-123") {
		t.Errorf("Expected server-generated request ID, but client ID was used: %s", string(body))
	}
	if !strings.Contains(string(body), "<RequestID>") {
		t.Errorf("Expected RequestID in response, got: %s", string(body))
	}
}

// --- Context Propagation Tests ---

func TestSigV4Auth_ContextPropagation(t *testing.T) {
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			testAccessKey: {
				AccessKeyID:     testAccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "alice",
				AccountID:       "123456789012",
				Status:          "Active",
			},
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		identity := r.Context().Value(ctxIdentity)
		accountId := r.Context().Value(ctxAccountID)
		service := r.Context().Value(ctxService)
		region := r.Context().Value(ctxRegion)
		accessKey := r.Context().Value(ctxAccessKey)
		fmt.Fprintf(w,
			"identity=%v|accountId=%v|service=%v|region=%v|accessKey=%v",
			identity, accountId, service, region, accessKey,
		)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

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
		{"accessKey", "accessKey=" + testAccessKey},
	}
	for _, check := range checks {
		if !strings.Contains(result, check.expected) {
			t.Errorf("Expected %s in response, got: %s", check.expected, result)
		}
	}
}

func TestSigV4Auth_ContextDoesNotLeakBetweenRequests(t *testing.T) {
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	secondKey := "AKIAIOSFODNN7SECOND0"
	encryptedSecret2, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			testAccessKey: {
				AccessKeyID:     testAccessKey,
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

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		identity := r.Context().Value(ctxIdentity)
		accountID := r.Context().Value(ctxAccountID)
		fmt.Fprintf(w, "%v:%v", identity, accountID)
	})

	// First request as alice
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Host = "localhost:9999"
	signTestRequest(t, req1, nil, testAccessKey, testSecretKey)

	resp1 := doRequest(r, req1)
	body1, _ := io.ReadAll(resp1.Body)
	if string(body1) != "alice:111111111111" {
		t.Errorf("Request 1: expected alice:111111111111, got %s", string(body1))
	}

	// Second request as bob
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Host = "localhost:9999"
	signTestRequest(t, req2, nil, secondKey, testSecretKey)

	resp2 := doRequest(r, req2)
	body2, _ := io.ReadAll(resp2.Body)
	if string(body2) != "bob:222222222222" {
		t.Errorf("Request 2: expected bob:222222222222, got %s", string(body2))
	}
}

// --- LookupAccessKey Error Variants ---

func TestSigV4Auth_LookupAccessKeyUnexpectedError(t *testing.T) {
	// A non-NoSuchEntity error from LookupAccessKey should yield 500 InternalError.
	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     &errorLookupMockIAMService{err: errors.New("nats: connection closed")},
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

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
	handler := setupTestApp(testAccessKey, testSecretKey)

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
			signTestRequest(t, req, nil, testAccessKey, testSecretKey)

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
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			testAccessKey: {
				AccessKeyID:     testAccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "root",
				Status:          "Active",
			},
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	body := []byte("Action=DescribeInstances")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signTestRequest(t, req, body, testAccessKey, testSecretKey)

	resp := doRequest(r, req)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(respBody))
	}
}

func TestSigV4Auth_EmptyBodyPOST(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	// POST with empty body — payload hash is hash of ""
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestSigV4Auth_MultipartContentType(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	body := []byte("Action=DescribeInstances")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "multipart/form-data; boundary=something")
	signTestRequest(t, req, body, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200 for multipart content-type, got %d, body: %s", resp.StatusCode, string(respBody))
	}
}

func TestSigV4Auth_QueryStringEdgeCases(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

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
			signTestRequest(t, req, nil, testAccessKey, testSecretKey)

			resp := doRequest(handler, req)

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
			}
		})
	}
}

// --- Policy Enforcement Tests (checkPolicy) ---

// policyMockIAMService extends mockIAMService with configurable GetUserPolicies
// and GetRolePolicies resolvers, so checkPolicy tests can drive both the user
// and assumed-role enforcement branches.
type policyMockIAMService struct {
	mockIAMService

	getUserPoliciesFn func(accountID, userName string) ([]handlers_iam.PolicyDocument, error)
	getRolePoliciesFn func(accountID, roleName string) ([]handlers_iam.PolicyDocument, error)
}

func (m *policyMockIAMService) GetUserPolicies(accountID, userName string) ([]handlers_iam.PolicyDocument, error) {
	if m.getUserPoliciesFn != nil {
		return m.getUserPoliciesFn(accountID, userName)
	}
	return nil, nil
}

func (m *policyMockIAMService) GetRolePolicies(accountID, roleName string) ([]handlers_iam.PolicyDocument, error) {
	if m.getRolePoliciesFn != nil {
		return m.getRolePoliciesFn(accountID, roleName)
	}
	return nil, nil
}

// errorLookupMockIAMService returns a configurable error from LookupAccessKey.
type errorLookupMockIAMService struct {
	mockIAMService

	err error
}

func (m *errorLookupMockIAMService) LookupAccessKey(_ string) (*handlers_iam.AccessKey, error) {
	return nil, m.err
}

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
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &policyMockIAMService{
		mockIAMService: mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {
					AccessKeyID:     testAccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return nil, nil // no policies
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

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
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &policyMockIAMService{
		mockIAMService: mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {
					AccessKeyID:     testAccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return []handlers_iam.PolicyDocument{
				{
					Version: "2012-10-17",
					Statement: []handlers_iam.Statement{
						{
							Effect:   "Allow",
							Action:   handlers_iam.StringOrArr{"ec2:RunInstances"},
							Resource: handlers_iam.StringOrArr{"*"},
						},
					},
				},
			}, nil
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestCheckPolicy_NonRootWithExplicitDeny_Denied(t *testing.T) {
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &policyMockIAMService{
		mockIAMService: mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {
					AccessKeyID:     testAccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return []handlers_iam.PolicyDocument{
				{
					Version: "2012-10-17",
					Statement: []handlers_iam.Statement{
						{
							Effect:   "Allow",
							Action:   handlers_iam.StringOrArr{"ec2:*"},
							Resource: handlers_iam.StringOrArr{"*"},
						},
						{
							Effect:   "Deny",
							Action:   handlers_iam.StringOrArr{"ec2:RunInstances"},
							Resource: handlers_iam.StringOrArr{"*"},
						},
					},
				},
			}, nil
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

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
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &policyMockIAMService{
		mockIAMService: mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {
					AccessKeyID:     testAccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "root",
					AccountID:       utils.GlobalAccountID,
					Status:          "Active",
				},
			},
		},
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			// Return explicit deny — root should bypass anyway
			return []handlers_iam.PolicyDocument{
				{
					Version: "2012-10-17",
					Statement: []handlers_iam.Statement{
						{
							Effect:   "Deny",
							Action:   handlers_iam.StringOrArr{"*"},
							Resource: handlers_iam.StringOrArr{"*"},
						},
					},
				},
			}, nil
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 200 (root bypass), got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestCheckPolicy_MissingAccountID_InternalError(t *testing.T) {
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	// AccessKey with empty AccountID — checkPolicy should return InternalError
	mockSvc := &policyMockIAMService{
		mockIAMService: mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {
					AccessKeyID:     testAccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "", // empty
					Status:          "Active",
				},
			},
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 500, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestCheckPolicy_GetUserPoliciesError_InternalError(t *testing.T) {
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &policyMockIAMService{
		mockIAMService: mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {
					AccessKeyID:     testAccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return nil, errors.New("nats: timeout")
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 500, got %d, body: %s", resp.StatusCode, string(body))
	}
}

func TestCheckPolicy_RootNonGlobalAccount_StillEvaluated(t *testing.T) {
	// A user named "root" but on a non-global account should still have
	// policies evaluated (only root + GlobalAccountID bypasses).
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("Failed to encrypt secret: %v", err)
	}

	mockSvc := &policyMockIAMService{
		mockIAMService: mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {
					AccessKeyID:     testAccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "root",
					AccountID:       "999999999999", // not GlobalAccountID
					Status:          "Active",
				},
			},
		},
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return nil, nil // no policies → should deny
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
	}

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)

	// root on non-global account is evaluated by the policy engine like any
	// other user. With no policies attached, the default deny applies.
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("Expected status 403, got %d, body: %s", resp.StatusCode, string(body))
	}
}

// TestSigV4Auth_RequireSignedHeaders verifies that omitting "host" or
// "x-amz-date" from SignedHeaders is rejected with IncompleteSignature before
// signature comparison. Omitting either enables Authorization header replay
// across vhosts or outside the clock-skew window.
func TestSigV4Auth_RequireSignedHeaders(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

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
			signTestRequest(t, req, nil, testAccessKey, testSecretKey)
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

// TestSigV4Auth_NATSDisconnectedShortCircuit: with nil IAMService a post-lookup
// failure would be 500; a 503 here proves the NATS short-circuit fired first.
func TestSigV4Auth_NATSDisconnectedShortCircuit(t *testing.T) {
	ns, _ := testutil.StartTestNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	nc.Close() // disconnected — IsConnected() returns false

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		NATSConn:       nc,
		IAMService:     nil,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

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

// mockSTSService implements handlers_sts.STSService for auth-middleware tests.
// Only LookupSessionCredential and VerifySessionToken are exercised; others
// return nil so a misrouted call panics instead of silently allowing.
type mockSTSService struct {
	sessions  map[string]*handlers_sts.SessionCredential
	tokens    map[string]string // AKID → plaintext wire token for HMAC equivalence
	lookupErr error
	lookups   atomic.Int32 // counts LookupSessionCredential calls for negative-side-effect assertions
}

func (m *mockSTSService) AssumeRole(_, _, _ string, _ *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
	return nil, nil
}
func (m *mockSTSService) AssumeRoleForInstance(_, _, _ string, _ int64) (*sts.AssumeRoleOutput, error) {
	return nil, nil
}
func (m *mockSTSService) GetCallerIdentity(_, _, _ string, _ *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
	return nil, nil
}
func (m *mockSTSService) GetSessionToken(_, _, _, _ string, _ *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
	return nil, nil
}
func (m *mockSTSService) LookupSessionCredential(accessKeyID string) (*handlers_sts.SessionCredential, error) {
	m.lookups.Add(1)
	if m.lookupErr != nil {
		return nil, m.lookupErr
	}
	if !strings.HasPrefix(accessKeyID, "ASIA") {
		return nil, nil
	}
	cred, ok := m.sessions[accessKeyID]
	if !ok {
		return nil, nil
	}
	return cred, nil
}

// VerifySessionToken uses plaintext equality rather than HMAC to keep tests
// simple while preserving the contract shape.
func (m *mockSTSService) VerifySessionToken(cred *handlers_sts.SessionCredential, wireToken string) bool {
	if cred == nil {
		return false
	}
	want, ok := m.tokens[cred.AccessKeyID]
	if !ok {
		return false
	}
	return want == wireToken
}

func (m *mockSTSService) AssumeRoleWithWebIdentity(_ *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

func (m *mockSTSService) VerifyPresignedGetCallerIdentity(_, _ string) (*handlers_sts.PresignedCallerIdentity, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

const (
	testSessionAKID  = "ASIATESTSESSIONAAAAA"
	testSessionToken = "wire-session-token-plain"
	testRoleARN      = "arn:aws:iam::123456789012:role/test-role"
	testAssumedARN   = "arn:aws:sts::123456789012:assumed-role/test-role/test-session"
)

// setupSessionTestApp wires a gateway with IAM+STS mocks. The stored secret is
// real AES-GCM so the SigV4 verify path exercises the actual decrypt code.
func setupSessionTestApp(t *testing.T, expiresAt time.Time) (http.Handler, *mockSTSService) {
	t.Helper()
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)

	cred := &handlers_sts.SessionCredential{
		AccessKeyID:       testSessionAKID,
		SecretEncrypted:   encryptedSecret,
		SessionTokenHMAC:  "ignored-in-mock", // mock verifies by plaintext map
		AccountID:         "123456789012",
		PrincipalType:     principalTypeAssumedRole,
		AssumedRoleARN:    testAssumedARN,
		UnderlyingRoleARN: testRoleARN,
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
func mountSessionGateway(t *testing.T, cred *handlers_sts.SessionCredential) (http.Handler, *mockSTSService) {
	t.Helper()
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)

	stsMock := &mockSTSService{
		sessions: map[string]*handlers_sts.SessionCredential{cred.AccessKeyID: cred},
		tokens:   map[string]string{cred.AccessKeyID: testSessionToken},
	}

	iamMock := &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			testAccessKey: {
				AccessKeyID:     testAccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "alice",
				AccountID:       "999999999999",
				Status:          "Active",
			},
		},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     iamMock,
		STSService:     stsMock,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		fmt.Fprintf(w,
			"identity=%v|accountId=%v|principalType=%v|assumedRoleARN=%v",
			ctx.Value(ctxIdentity),
			ctx.Value(ctxAccountID),
			ctx.Value(ctxPrincipalType),
			ctx.Value(ctxAssumedRoleARN),
		)
	})
	return r, stsMock
}

// signSessionRequest sets X-Amz-Security-Token before signing so it's included
// in SignedHeaders, matching what the AWS CLI/SDK do with session credentials.
func signSessionRequest(t *testing.T, req *http.Request, body []byte, accessKey, secret, sessionToken string) {
	t.Helper()
	req.Header.Set("X-Amz-Security-Token", sessionToken)
	signTestRequest(t, req, body, accessKey, secret)
}

func TestSigV4Auth_Session_ValidSignature(t *testing.T) {
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signSessionRequest(t, req, nil, testSessionAKID, testSecretKey, testSessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "identity=test-session")
	assert.Contains(t, bodyStr, "accountId=123456789012")
	assert.Contains(t, bodyStr, "principalType=assumed-role")
	assert.Contains(t, bodyStr, "assumedRoleARN="+testAssumedARN)
}

func TestSigV4Auth_Session_UserPrincipal(t *testing.T) {
	// GetSessionToken session (PrincipalType "user") must resolve back to the IAM
	// user with no assumed-role fields, so buildCallerARN yields arn:aws:iam::A:user/N.
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	cred := &handlers_sts.SessionCredential{
		AccessKeyID:      testSessionAKID,
		SecretEncrypted:  encryptedSecret,
		SessionTokenHMAC: "ignored-in-mock",
		AccountID:        "123456789012",
		PrincipalType:    principalTypeUser,
		SessionName:      "alice",
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
		CreatedAt:        time.Now().UTC().Add(-time.Minute),
	}
	handler, _ := mountSessionGateway(t, cred)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signSessionRequest(t, req, nil, testSessionAKID, testSecretKey, testSessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "identity=alice")
	assert.Contains(t, bodyStr, "accountId=123456789012")
	assert.Contains(t, bodyStr, "principalType=user")
	// A user session carries no assumed-role ARN, so the context value is unset.
	assert.Contains(t, bodyStr, "assumedRoleARN=<nil>")
}

func TestSigV4Auth_Session_EmptyPrincipalType_ResolvesAssumedRole(t *testing.T) {
	// Pre-PrincipalType records have an empty value and must still resolve as assumed-role.
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	cred := &handlers_sts.SessionCredential{
		AccessKeyID:       testSessionAKID,
		SecretEncrypted:   encryptedSecret,
		SessionTokenHMAC:  "ignored-in-mock",
		AccountID:         "123456789012",
		PrincipalType:     "", // pre-migration record
		AssumedRoleARN:    testAssumedARN,
		UnderlyingRoleARN: testRoleARN,
		RoleID:            "AROAEXAMPLEAAAAAA",
		AssumedRoleID:     "AROAEXAMPLEAAAAAA:test-session",
		SessionName:       "test-session",
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		CreatedAt:         time.Now().UTC().Add(-time.Minute),
	}
	handler, _ := mountSessionGateway(t, cred)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signSessionRequest(t, req, nil, testSessionAKID, testSecretKey, testSessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "identity=test-session")
	assert.Contains(t, bodyStr, "principalType=assumed-role")
	assert.Contains(t, bodyStr, "assumedRoleARN="+testAssumedARN)
}

func TestSigV4Auth_Session_Expired(t *testing.T) {
	// Past expiry but within janitor grace window: must reject as ExpiredToken.
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(-time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signSessionRequest(t, req, nil, testSessionAKID, testSecretKey, testSessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(body), "ExpiredToken")
}

func TestSigV4Auth_Session_WrongSecurityToken(t *testing.T) {
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signSessionRequest(t, req, nil, testSessionAKID, testSecretKey, "wrong-wire-token")

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "InvalidClientTokenId")
}

func TestSigV4Auth_Session_MissingSecurityToken(t *testing.T) {
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	// No X-Amz-Security-Token — ASIA path requires it.
	signTestRequest(t, req, nil, testSessionAKID, testSecretKey)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "InvalidClientTokenId")
}

func TestSigV4Auth_Session_AKIDNotInBucket(t *testing.T) {
	handler, _ := setupSessionTestApp(t, time.Now().UTC().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signSessionRequest(t, req, nil, "ASIAOTHERAKIDAAAAAAA", testSecretKey, testSessionToken)

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
	signTestRequest(t, req, nil, "ZZIATESTAKIDXXXXXXXX", testSecretKey)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "InvalidClientTokenId")
	assert.Equal(t, int32(0), stsMock.lookups.Load(), "STSService must not be consulted for unknown-prefix AKIDs")
}

func TestSigV4Auth_AKIA_PrincipalTypeUser(t *testing.T) {
	// Long-lived AKIA path must set ctxPrincipalType=user for correct policy gating.
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)

	iamMock := &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			testAccessKey: {
				AccessKeyID:     testAccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "alice",
				AccountID:       "111111111111",
				Status:          "Active",
			},
		},
	}
	gw := &GatewayConfig{DisableLogging: true, Region: testRegion, IAMService: iamMock}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "principalType=%v|assumedRoleARN=%v",
			req.Context().Value(ctxPrincipalType),
			req.Context().Value(ctxAssumedRoleARN))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)
	resp := doRequest(r, req)
	body, _ := io.ReadAll(resp.Body)

	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	assert.Contains(t, string(body), "principalType=user")
	// ctxAssumedRoleARN unset → "<nil>" via fmt %v
	assert.Contains(t, string(body), "assumedRoleARN=<nil>")
}

func TestSigV4Auth_Session_STSServiceNil(t *testing.T) {
	// Session AKID with nil STSService is a misconfiguration — must fail with InternalError.
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	iamMock := &mockIAMService{
		masterKey:  testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{},
	}
	_ = encryptedSecret
	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     iamMock,
		STSService:     nil,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signSessionRequest(t, req, nil, testSessionAKID, testSecretKey, testSessionToken)

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
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)

	var userPoliciesCalled bool
	var resolvedRoleName string

	// If SessionName were ever used as the policy-lookup key, the request would
	// inherit alice's ec2:* permissions.
	iamMock := &policyMockIAMService{
		mockIAMService: mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {
					AccessKeyID:     testAccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       "123456789012",
					Status:          "Active",
				},
			},
		},
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			userPoliciesCalled = true
			return allowPolicy("ec2:*"), nil
		},
		getRolePoliciesFn: func(_, roleName string) ([]handlers_iam.PolicyDocument, error) {
			resolvedRoleName = roleName
			return nil, nil
		},
	}

	cred := &handlers_sts.SessionCredential{
		AccessKeyID:       testSessionAKID,
		SecretEncrypted:   encryptedSecret,
		AccountID:         "123456789012",
		PrincipalType:     principalTypeAssumedRole,
		AssumedRoleARN:    "arn:aws:sts::123456789012:assumed-role/some-role/alice",
		UnderlyingRoleARN: "arn:aws:iam::123456789012:role/some-role",
		SessionName:       "alice",
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		CreatedAt:         time.Now().UTC().Add(-time.Minute),
	}
	stsMock := &mockSTSService{
		sessions: map[string]*handlers_sts.SessionCredential{testSessionAKID: cred},
		tokens:   map[string]string{testSessionAKID: testSessionToken},
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     iamMock,
		STSService:     stsMock,
	}

	handler := setupPolicyTestHandler(gw)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signSessionRequest(t, req, nil, testSessionAKID, testSecretKey, testSessionToken)

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "AccessDenied")
	assert.Equal(t, "some-role", resolvedRoleName)
	assert.False(t, userPoliciesCalled, "priv-esc: assumed-role session must not consult GetUserPolicies")
}

// allowPolicy returns a single-statement Allow policy granting action on "*".
func allowPolicy(action string) []handlers_iam.PolicyDocument {
	return allowPolicyResource(action, "*")
}

// allowPolicyResource returns a single-statement Allow policy for action on resource.
func allowPolicyResource(action, resource string) []handlers_iam.PolicyDocument {
	return []handlers_iam.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{{
			Effect:   "Allow",
			Action:   handlers_iam.StringOrArr{action},
			Resource: handlers_iam.StringOrArr{resource},
		}},
	}}
}

// assumedRoleSessionCred builds an assumed-role SessionCredential.
// SecretEncrypted is filled in by the gateway builder.
func assumedRoleSessionCred(sessionName, underlyingRoleARN, accountID string) *handlers_sts.SessionCredential {
	return &handlers_sts.SessionCredential{
		AccessKeyID:       testSessionAKID,
		AccountID:         accountID,
		PrincipalType:     principalTypeAssumedRole,
		AssumedRoleARN:    "arn:aws:sts::" + accountID + ":assumed-role/role/" + sessionName,
		UnderlyingRoleARN: underlyingRoleARN,
		SessionName:       sessionName,
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		CreatedAt:         time.Now().UTC().Add(-time.Minute),
	}
}

// newAssumedRoleEnforcementGateway wires a gateway for assumed-role checkPolicy tests.
// The user resolver fails the test if called — an assumed-role must never be evaluated as a user.
func newAssumedRoleEnforcementGateway(t *testing.T, cred *handlers_sts.SessionCredential,
	getRoleFn func(accountID, roleName string) ([]handlers_iam.PolicyDocument, error),
) *GatewayConfig {
	t.Helper()
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	cred.SecretEncrypted = encryptedSecret

	iamMock := &policyMockIAMService{
		mockIAMService: mockIAMService{masterKey: testMasterKey},
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			t.Errorf("priv-esc: assumed-role principal must not consult GetUserPolicies")
			return nil, nil
		},
		getRolePoliciesFn: getRoleFn,
	}
	stsMock := &mockSTSService{
		sessions: map[string]*handlers_sts.SessionCredential{cred.AccessKeyID: cred},
		tokens:   map[string]string{cred.AccessKeyID: testSessionToken},
	}
	return &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     iamMock,
		STSService:     stsMock,
	}
}

// setupPolicyResourceTestHandler is setupPolicyTestHandler for resource-scoped
// checks (e.g. iam:PassRole on a specific role ARN).
func setupPolicyResourceTestHandler(gw *GatewayConfig, service, action, resource string) http.Handler {
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
		if err := gw.checkPolicyResource(req, service, action, resource); err != nil {
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
	signSessionRequest(t, req, nil, akid, testSecretKey, testSessionToken)
	return doRequest(handler, req)
}

// TestCheckPolicy_AssumedRole_AllowedByRolePolicy: the underlying role's managed
// policy grants the action, so the session is allowed — the symmetric twin of
// predastore's assumed-role S3 enforcement.
func TestCheckPolicy_AssumedRole_AllowedByRolePolicy(t *testing.T) {
	cred := assumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, roleName string) ([]handlers_iam.PolicyDocument, error) {
		require.Equal(t, "app-role", roleName)
		return allowPolicy("ec2:RunInstances"), nil
	})

	resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
}

// TestCheckPolicy_AssumedRole_ImplicitDeny: the role policy matches neither the
// action nor an explicit Deny, so the default deny applies.
func TestCheckPolicy_AssumedRole_ImplicitDeny(t *testing.T) {
	cred := assumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		return allowPolicy("ec2:DescribeInstances"), nil // does not cover RunInstances
	})

	resp := doSessionPolicyRequest(t, setupPolicyTestHandler(gw), cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "AccessDenied")
}

// TestCheckPolicy_AssumedRole_ExplicitDeny: an explicit Deny in the role policy
// overrides any Allow.
func TestCheckPolicy_AssumedRole_ExplicitDeny(t *testing.T) {
	cred := assumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		return []handlers_iam.PolicyDocument{{
			Version: "2012-10-17",
			Statement: []handlers_iam.Statement{
				{Effect: "Allow", Action: handlers_iam.StringOrArr{"ec2:*"}, Resource: handlers_iam.StringOrArr{"*"}},
				{Effect: "Deny", Action: handlers_iam.StringOrArr{"ec2:RunInstances"}, Resource: handlers_iam.StringOrArr{"*"}},
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
	cred := assumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
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
	cred := assumedRoleSessionCred("root",
		"arn:aws:iam::"+utils.GlobalAccountID+":role/sneaky-role", utils.GlobalAccountID)
	var roleResolved bool
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, roleName string) ([]handlers_iam.PolicyDocument, error) {
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
			cred := assumedRoleSessionCred("app", tc.underlyingRoleARN, "123456789012")
			gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
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
	cred := assumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
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
	cred := assumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/app-role", "123456789012")
	var calls int
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
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
	cred := assumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/launcher-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		return allowPolicyResource("iam:PassRole", targetRoleARN), nil
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
	cred := assumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/launcher-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		return allowPolicyResource("iam:PassRole", "arn:aws:iam::123456789012:role/some-other-role"), nil
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
	cred := assumedRoleSessionCred("app", "arn:aws:iam::123456789012:role/launcher-role", "123456789012")
	gw := newAssumedRoleEnforcementGateway(t, cred, func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		return allowPolicyResource("iam:PassRole", "*"), nil
	})

	handler := setupPolicyResourceTestHandler(gw, "iam", "PassRole", targetRoleARN)
	resp := doSessionPolicyRequest(t, handler, cred.AccessKeyID)
	body, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
}

func (m *mockIAMService) TagUser(_ string, _ *iam.TagUserInput) (*iam.TagUserOutput, error) {
	return &iam.TagUserOutput{}, nil
}
func (m *mockIAMService) UntagUser(_ string, _ *iam.UntagUserInput) (*iam.UntagUserOutput, error) {
	return &iam.UntagUserOutput{}, nil
}
func (m *mockIAMService) ListUserTags(_ string, _ *iam.ListUserTagsInput) (*iam.ListUserTagsOutput, error) {
	return &iam.ListUserTagsOutput{}, nil
}
func (m *mockIAMService) TagRole(_ string, _ *iam.TagRoleInput) (*iam.TagRoleOutput, error) {
	return &iam.TagRoleOutput{}, nil
}
func (m *mockIAMService) UntagRole(_ string, _ *iam.UntagRoleInput) (*iam.UntagRoleOutput, error) {
	return &iam.UntagRoleOutput{}, nil
}
func (m *mockIAMService) ListRoleTags(_ string, _ *iam.ListRoleTagsInput) (*iam.ListRoleTagsOutput, error) {
	return &iam.ListRoleTagsOutput{}, nil
}
func (m *mockIAMService) TagPolicy(_ string, _ *iam.TagPolicyInput) (*iam.TagPolicyOutput, error) {
	return &iam.TagPolicyOutput{}, nil
}
func (m *mockIAMService) UntagPolicy(_ string, _ *iam.UntagPolicyInput) (*iam.UntagPolicyOutput, error) {
	return &iam.UntagPolicyOutput{}, nil
}
func (m *mockIAMService) ListPolicyTags(_ string, _ *iam.ListPolicyTagsInput) (*iam.ListPolicyTagsOutput, error) {
	return &iam.ListPolicyTagsOutput{}, nil
}
func (m *mockIAMService) TagInstanceProfile(_ string, _ *iam.TagInstanceProfileInput) (*iam.TagInstanceProfileOutput, error) {
	return &iam.TagInstanceProfileOutput{}, nil
}
func (m *mockIAMService) UntagInstanceProfile(_ string, _ *iam.UntagInstanceProfileInput) (*iam.UntagInstanceProfileOutput, error) {
	return &iam.UntagInstanceProfileOutput{}, nil
}
func (m *mockIAMService) ListInstanceProfileTags(_ string, _ *iam.ListInstanceProfileTagsInput) (*iam.ListInstanceProfileTagsOutput, error) {
	return &iam.ListInstanceProfileTagsOutput{}, nil
}
func (m *mockIAMService) TagOpenIDConnectProvider(_ string, _ *iam.TagOpenIDConnectProviderInput) (*iam.TagOpenIDConnectProviderOutput, error) {
	return &iam.TagOpenIDConnectProviderOutput{}, nil
}
func (m *mockIAMService) UntagOpenIDConnectProvider(_ string, _ *iam.UntagOpenIDConnectProviderInput) (*iam.UntagOpenIDConnectProviderOutput, error) {
	return &iam.UntagOpenIDConnectProviderOutput{}, nil
}
func (m *mockIAMService) ListOpenIDConnectProviderTags(_ string, _ *iam.ListOpenIDConnectProviderTagsInput) (*iam.ListOpenIDConnectProviderTagsOutput, error) {
	return &iam.ListOpenIDConnectProviderTagsOutput{}, nil
}
