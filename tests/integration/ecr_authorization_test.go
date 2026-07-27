//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pullOnlyPolicyARN and readOnlyPolicyARN are the AWS-managed policies this
// suite attaches to IAM users/roles to prove the gateway's ECR authorization
// matrix for each grant level.
const (
	pullOnlyPolicyARN = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly"
	readOnlyPolicyARN = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
)

// ociManifestMediaType is the content type used for the minimal image
// manifests this suite pushes to seed pull fixtures.
const ociManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"

// ociDigest returns the sha256: digest of data.
func ociDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// uniqueName returns a collision-resistant name scoped to a test.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// ecrGetLoginPassword returns the decoded registry password (the JWT half of
// the authorization token) for the AWS:<jwt> credential docker login expects.
func ecrGetLoginPassword(t *testing.T, c *ecr.ECR) string {
	t.Helper()
	out, err := c.GetAuthorizationToken(&ecr.GetAuthorizationTokenInput{})
	require.NoError(t, err)
	require.NotEmpty(t, out.AuthorizationData, "no authorization data returned")
	raw, err := base64.StdEncoding.DecodeString(aws.StringValue(out.AuthorizationData[0].AuthorizationToken))
	require.NoError(t, err)
	user, pass, found := strings.Cut(string(raw), ":")
	require.True(t, found && user == "AWS", "unexpected authorization token shape")
	return pass
}

// ecrOCIRequest issues a raw HTTP request against the gateway's OCI
// Distribution Spec v2 surface (mounted at /v2/*), authenticating with
// bearerToken. Unlike the AWS SDK calls in this file, /v2/* is not
// SigV4-signed — the gateway authenticates it purely via the Bearer JWT — so
// this issues the request directly against the httptest server rather than
// through a signer.
func ecrOCIRequest(t *testing.T, gw *Gateway, method, path, bearerToken string, body []byte) (status int, headers http.Header, respBody []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, gw.Server.URL+path, reader)
	require.NoError(t, err, "build OCI request")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := gw.Server.Client().Do(req)
	require.NoError(t, err, "%s %s", method, path)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return resp.StatusCode, resp.Header, respBody
}

// seedOCIRepo creates repo as the seeded root administrator and pushes one
// minimal image (an empty-layer manifest referencing a single config blob)
// tagged "v1", giving the authorization-matrix subtests something to pull.
// Returns the manifest digest.
func seedOCIRepo(t *testing.T, gw *Gateway, repo string) string {
	t.Helper()
	_, err := gw.ECRClient(t).CreateRepository(&ecr.CreateRepositoryInput{RepositoryName: aws.String(repo)})
	require.NoError(t, err, "create-repository")
	adminBearer := ecrGetLoginPassword(t, gw.ECRClient(t))

	cfg := []byte("iam-authorization-integration-config")
	cfgDigest := ociDigest(cfg)

	status, hdr, body := ecrOCIRequest(t, gw, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", adminBearer, nil)
	require.Equal(t, http.StatusAccepted, status, "start upload: %s", body)
	loc := hdr.Get("Location")
	require.NotEmpty(t, loc, "upload response missing Location header")

	status, _, body = ecrOCIRequest(t, gw, http.MethodPut, loc+"?digest="+cfgDigest, adminBearer, cfg)
	require.Equal(t, http.StatusCreated, status, "finish upload: %s", body)

	manifest := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`,
		ociManifestMediaType, cfgDigest)
	status, hdr, body = ecrOCIRequest(t, gw, http.MethodPut, "/v2/"+repo+"/manifests/v1", adminBearer, manifest)
	require.Equal(t, http.StatusCreated, status, "push manifest: %s", body)
	return hdr.Get("Docker-Content-Digest")
}

// newScopedECRUser creates a fresh IAM user with policyARN attached and one
// active access key. Returns PrincipalClients bound to that key so callers
// can mint ECR bearer tokens as this identity.
func newScopedECRUser(t *testing.T, gw *Gateway, policyARN string) *PrincipalClients {
	t.Helper()
	userName := uniqueName("ecr-authz")

	_, err := gw.IAMClient(t).CreateUser(&iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-user")

	_, err = gw.IAMClient(t).AttachUserPolicy(&iam.AttachUserPolicyInput{
		UserName: aws.String(userName), PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "attach-user-policy")

	keyOut, err := gw.IAMClient(t).CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-access-key")

	return gw.ClientsWithCreds(t, aws.StringValue(keyOut.AccessKey.AccessKeyId), aws.StringValue(keyOut.AccessKey.SecretAccessKey))
}

// assertPullAllowedPushDenied runs the read/write half of the release-gate
// matrix common to every scoped-credential subtest below: pulling repo's
// seeded "v1" tag must succeed, while every mutating operation must return
// 403 DENIED without ever reaching the object store.
func assertPullAllowedPushDenied(t *testing.T, gw *Gateway, cli *PrincipalClients, repo, manifestDigest string) {
	t.Helper()
	bearer := ecrGetLoginPassword(t, cli.ECR)

	status, _, body := ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/manifests/v1", bearer, nil)
	assert.Equal(t, http.StatusOK, status, "pull manifest: %s", body)

	status, _, body = ecrOCIRequest(t, gw, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", bearer, nil)
	assert.Equal(t, http.StatusForbidden, status, "push must be denied: %s", body)

	status, _, body = ecrOCIRequest(t, gw, http.MethodPut, "/v2/"+repo+"/manifests/v1", bearer,
		[]byte(`{"schemaVersion":2,"mediaType":"`+ociManifestMediaType+`","config":{"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"},"layers":[]}`))
	assert.Equal(t, http.StatusForbidden, status, "manifest overwrite must be denied: %s", body)

	status, _, body = ecrOCIRequest(t, gw, http.MethodDelete, "/v2/"+repo+"/manifests/"+manifestDigest, bearer, nil)
	assert.Equal(t, http.StatusForbidden, status, "manifest delete must be denied: %s", body)
}

// TestECRIAMAuthorization_AssumedRolePullOnly proves the pull-allow /
// push-deny matrix holds for a token minted from ASIA (assumed-role) session
// credentials, exercising the same evaluatePrincipalPolicy authorization
// path the OCI registry surface shares with the SigV4 API.
func TestECRIAMAuthorization_AssumedRolePullOnly(t *testing.T) {
	gw := StartGateway(t)
	StartECRDaemonLite(t, gw)

	repo := uniqueRepo("authz-role-pullonly")
	manifestDigest := seedOCIRepo(t, gw, repo)

	roleName := uniqueName("ecr-authz-role")
	_, err := gw.IAMClient(t).CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(assumedRoleTrustPolicy),
	})
	require.NoError(t, err, "create-role")

	_, err = gw.IAMClient(t).AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName: aws.String(roleName), PolicyArn: aws.String(pullOnlyPolicyARN),
	})
	require.NoError(t, err, "attach-role-policy")

	assumed, err := gw.STSClient(t).AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(iamRoleARN(gw.AccountID, roleName)),
		RoleSessionName: aws.String(uniqueName("authz-session")),
	})
	require.NoError(t, err, "assume-role")
	creds := assumed.Credentials
	require.NotNil(t, creds, "AssumeRole returned nil Credentials")
	cli := gw.ClientsWithSessionCreds(t,
		aws.StringValue(creds.AccessKeyId), aws.StringValue(creds.SecretAccessKey), aws.StringValue(creds.SessionToken))

	assertPullAllowedPushDenied(t, gw, cli, repo, manifestDigest)
}

// TestECRIAMAuthorization_PullOnly proves a PullOnly-scoped identity can pull
// but not push, overwrite, or delete, and cannot list the account catalog
// (DescribeRepositories is outside PullOnly's grant).
func TestECRIAMAuthorization_PullOnly(t *testing.T) {
	gw := StartGateway(t)
	StartECRDaemonLite(t, gw)

	repo := uniqueRepo("authz-pullonly")
	manifestDigest := seedOCIRepo(t, gw, repo)

	cli := newScopedECRUser(t, gw, pullOnlyPolicyARN)
	assertPullAllowedPushDenied(t, gw, cli, repo, manifestDigest)

	bearer := ecrGetLoginPassword(t, cli.ECR)
	status, _, body := ecrOCIRequest(t, gw, http.MethodGet, "/v2/_catalog", bearer, nil)
	assert.Equal(t, http.StatusForbidden, status, "catalog listing outside PullOnly's grant must be denied: %s", body)
}

// TestECRIAMAuthorization_ReadOnly proves a ReadOnly-scoped identity can pull,
// list tags, and list the catalog, while push, overwrite, and delete remain
// denied.
func TestECRIAMAuthorization_ReadOnly(t *testing.T) {
	gw := StartGateway(t)
	StartECRDaemonLite(t, gw)

	repo := uniqueRepo("authz-readonly")
	manifestDigest := seedOCIRepo(t, gw, repo)

	cli := newScopedECRUser(t, gw, readOnlyPolicyARN)
	assertPullAllowedPushDenied(t, gw, cli, repo, manifestDigest)

	bearer := ecrGetLoginPassword(t, cli.ECR)
	status, _, body := ecrOCIRequest(t, gw, http.MethodGet, "/v2/_catalog", bearer, nil)
	assert.Equal(t, http.StatusOK, status, "catalog listing must be allowed under ReadOnly: %s", body)

	status, _, body = ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/tags/list", bearer, nil)
	assert.Equal(t, http.StatusOK, status, "tag listing must be allowed under ReadOnly: %s", body)
}

// TestECRIAMAuthorization_DetachPolicyDeniesImmediately proves a JWT minted
// while a policy was attached is denied on its very next request once that
// policy is detached — the token is a pointer re-resolved against live IAM
// state, not a capability baked in at mint time.
func TestECRIAMAuthorization_DetachPolicyDeniesImmediately(t *testing.T) {
	gw := StartGateway(t)
	StartECRDaemonLite(t, gw)

	repo := uniqueRepo("authz-detach")
	_ = seedOCIRepo(t, gw, repo)

	userName := uniqueName("ecr-authz-detach")
	_, err := gw.IAMClient(t).CreateUser(&iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-user")
	_, err = gw.IAMClient(t).AttachUserPolicy(&iam.AttachUserPolicyInput{
		UserName: aws.String(userName), PolicyArn: aws.String(pullOnlyPolicyARN),
	})
	require.NoError(t, err, "attach-user-policy")
	keyOut, err := gw.IAMClient(t).CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-access-key")
	cli := gw.ClientsWithCreds(t, aws.StringValue(keyOut.AccessKey.AccessKeyId), aws.StringValue(keyOut.AccessKey.SecretAccessKey))

	bearer := ecrGetLoginPassword(t, cli.ECR)
	status, _, body := ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/manifests/v1", bearer, nil)
	require.Equal(t, http.StatusOK, status, "pull must succeed while policy is attached: %s", body)

	_, err = gw.IAMClient(t).DetachUserPolicy(&iam.DetachUserPolicyInput{
		UserName: aws.String(userName), PolicyArn: aws.String(pullOnlyPolicyARN),
	})
	require.NoError(t, err, "detach-user-policy")

	status, _, body = ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/manifests/v1", bearer, nil)
	assert.Equal(t, http.StatusForbidden, status, "same JWT must be denied immediately once the policy is detached: %s", body)
}

// TestECRIAMAuthorization_DeactivatedKeyReturns401 proves a JWT minted from a
// since-deactivated access key is refused with 401 (invalid principal), not
// 403 — the key itself no longer authenticates, so the request never reaches
// policy evaluation.
func TestECRIAMAuthorization_DeactivatedKeyReturns401(t *testing.T) {
	gw := StartGateway(t)
	StartECRDaemonLite(t, gw)

	repo := uniqueRepo("authz-deactivate")
	_ = seedOCIRepo(t, gw, repo)

	userName := uniqueName("ecr-authz-deactivate")
	_, err := gw.IAMClient(t).CreateUser(&iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-user")
	_, err = gw.IAMClient(t).AttachUserPolicy(&iam.AttachUserPolicyInput{
		UserName: aws.String(userName), PolicyArn: aws.String(pullOnlyPolicyARN),
	})
	require.NoError(t, err, "attach-user-policy")
	keyOut, err := gw.IAMClient(t).CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-access-key")
	keyID := aws.StringValue(keyOut.AccessKey.AccessKeyId)
	cli := gw.ClientsWithCreds(t, keyID, aws.StringValue(keyOut.AccessKey.SecretAccessKey))

	bearer := ecrGetLoginPassword(t, cli.ECR)
	status, _, body := ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/manifests/v1", bearer, nil)
	require.Equal(t, http.StatusOK, status, "pull must succeed while the key is active: %s", body)

	_, err = gw.IAMClient(t).UpdateAccessKey(&iam.UpdateAccessKeyInput{
		UserName: aws.String(userName), AccessKeyId: aws.String(keyID), Status: aws.String(iam.StatusTypeInactive),
	})
	require.NoError(t, err, "update-access-key deactivate")

	status, hdr, body := ecrOCIRequest(t, gw, http.MethodGet, "/v2/"+repo+"/manifests/v1", bearer, nil)
	assert.Equal(t, http.StatusUnauthorized, status, "same JWT must be refused once its key is inactive: %s", body)
	assert.NotEmpty(t, hdr.Get("Www-Authenticate"), "a revoked-but-well-formed token still gets the Bearer challenge")
}
