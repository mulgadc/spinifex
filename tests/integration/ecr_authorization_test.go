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

// pullOnlyPolicyARN is the AWS-managed policy this suite attaches to an IAM
// role to prove the gateway's ECR authorization matrix for a pull-only
// grant.
const pullOnlyPolicyARN = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly"

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
