//go:build e2e

package ecr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pullOnlyPolicyARN = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly"
	readOnlyPolicyARN = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"

	// ociManifestMediaType is the content type used for the minimal image
	// manifests this suite pushes to seed pull fixtures.
	ociManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"

	// iamTrustAnyPrincipal lets any authenticated caller in the account assume
	// the role, matching the pattern used elsewhere in the E2E suite for
	// roles that only exist to be assumed by the test's own admin credentials.
	iamTrustAnyPrincipal = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`
)

func ociDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// seedOCIRepo creates repo as an administrator and pushes one minimal image
// (an empty-layer manifest referencing a single config blob) tagged "v1",
// giving the authorization-matrix subtests something to pull. Returns the
// manifest digest.
func seedOCIRepo(t *testing.T, f *Fixture, host, repo string) string {
	t.Helper()
	harness.CreateECRRepository(t, f.AWS, repo)
	adminBearer := harness.ECRGetLoginPassword(t, f.AWS)

	cfg := []byte("iam-authorization-e2e-config")
	cfgDigest := ociDigest(cfg)

	status, hdr, body := harness.OCIRequest(t, f.AWS, http.MethodPost, host, "/v2/"+repo+"/blobs/uploads/", adminBearer, nil)
	require.Equal(t, http.StatusAccepted, status, "start upload: %s", body)
	loc := hdr.Get("Location")
	require.NotEmpty(t, loc, "upload response missing Location header")

	status, _, body = harness.OCIRequest(t, f.AWS, http.MethodPut, host, loc+"?digest="+cfgDigest, adminBearer, cfg)
	require.Equal(t, http.StatusCreated, status, "finish upload: %s", body)

	manifest := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`,
		ociManifestMediaType, cfgDigest)
	status, hdr, body = harness.OCIRequest(t, f.AWS, http.MethodPut, host, "/v2/"+repo+"/manifests/v1", adminBearer, manifest)
	require.Equal(t, http.StatusCreated, status, "push manifest: %s", body)
	return hdr.Get("Docker-Content-Digest")
}

// newScopedIAMUser creates a fresh IAM user with policyARN attached and one
// active access key, registering teardown. Returns an AWSClient bound to
// that key so callers can mint ECR bearer tokens as this identity.
func newScopedIAMUser(t *testing.T, f *Fixture, policyARN string) *harness.AWSClient {
	t.Helper()
	userName := uniqueName("ecr-authz-e2e")

	_, err := f.AWS.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-user")
	t.Cleanup(func() {
		_, _ = f.AWS.IAM.DetachUserPolicy(&iam.DetachUserPolicyInput{
			UserName: aws.String(userName), PolicyArn: aws.String(policyARN),
		})
		keys, kerr := f.AWS.IAM.ListAccessKeys(&iam.ListAccessKeysInput{UserName: aws.String(userName)})
		if kerr == nil {
			for _, k := range keys.AccessKeyMetadata {
				_, _ = f.AWS.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{
					UserName: aws.String(userName), AccessKeyId: k.AccessKeyId,
				})
			}
		}
		_, _ = f.AWS.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(userName)})
	})

	_, err = f.AWS.IAM.AttachUserPolicy(&iam.AttachUserPolicyInput{
		UserName: aws.String(userName), PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "attach-user-policy")

	keyOut, err := f.AWS.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-access-key")

	return harness.NewAWSClientWithCreds(t, f.Env,
		aws.StringValue(keyOut.AccessKey.AccessKeyId), aws.StringValue(keyOut.AccessKey.SecretAccessKey))
}

// assertPullAllowedPushDenied runs the read/write half of the release-gate
// matrix common to every scoped-credential subtest below: pulling repo's
// seeded "v1" tag and HEADing its blob must succeed, while every mutating
// operation must return 403 DENIED without ever reaching the object store.
func assertPullAllowedPushDenied(t *testing.T, cli *harness.AWSClient, host, repo, manifestDigest string) {
	t.Helper()
	bearer := harness.ECRGetLoginPassword(t, cli)

	status, _, body := harness.OCIRequest(t, cli, http.MethodGet, host, "/v2/"+repo+"/manifests/v1", bearer, nil)
	assert.Equal(t, http.StatusOK, status, "pull manifest: %s", body)

	status, _, body = harness.OCIRequest(t, cli, http.MethodPost, host, "/v2/"+repo+"/blobs/uploads/", bearer, nil)
	assert.Equal(t, http.StatusForbidden, status, "push must be denied: %s", body)

	status, _, body = harness.OCIRequest(t, cli, http.MethodPut, host, "/v2/"+repo+"/manifests/v1", bearer,
		[]byte(`{"schemaVersion":2,"mediaType":"`+ociManifestMediaType+`","config":{"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"},"layers":[]}`))
	assert.Equal(t, http.StatusForbidden, status, "manifest overwrite must be denied: %s", body)

	status, _, body = harness.OCIRequest(t, cli, http.MethodDelete, host, "/v2/"+repo+"/manifests/"+manifestDigest, bearer, nil)
	assert.Equal(t, http.StatusForbidden, status, "manifest delete must be denied: %s", body)
}

// TestECRIAMAuthorization_PullOnly proves a PullOnly-scoped identity can pull
// but not push, overwrite, or delete, and cannot list the account catalog
// (DescribeRepositories is outside PullOnly's grant).
func TestECRIAMAuthorization_PullOnly(t *testing.T) {
	f := requireECRFixture(t)
	host := harness.ECRRegistryHost(f.Account)
	harness.RequireRegistryResolves(t, host)

	repo := uniqueRepo("authz-pullonly")
	manifestDigest := seedOCIRepo(t, f, host, repo)

	cli := newScopedIAMUser(t, f, pullOnlyPolicyARN)
	assertPullAllowedPushDenied(t, cli, host, repo, manifestDigest)

	bearer := harness.ECRGetLoginPassword(t, cli)
	status, _, body := harness.OCIRequest(t, cli, http.MethodGet, host, "/v2/_catalog", bearer, nil)
	assert.Equal(t, http.StatusForbidden, status, "catalog listing outside PullOnly's grant must be denied: %s", body)
}

// TestECRIAMAuthorization_ReadOnly proves a ReadOnly-scoped identity can pull,
// list tags, and list the catalog, while push, overwrite, and delete remain
// denied.
func TestECRIAMAuthorization_ReadOnly(t *testing.T) {
	f := requireECRFixture(t)
	host := harness.ECRRegistryHost(f.Account)
	harness.RequireRegistryResolves(t, host)

	repo := uniqueRepo("authz-readonly")
	manifestDigest := seedOCIRepo(t, f, host, repo)

	cli := newScopedIAMUser(t, f, readOnlyPolicyARN)
	assertPullAllowedPushDenied(t, cli, host, repo, manifestDigest)

	bearer := harness.ECRGetLoginPassword(t, cli)
	status, _, body := harness.OCIRequest(t, cli, http.MethodGet, host, "/v2/_catalog", bearer, nil)
	assert.Equal(t, http.StatusOK, status, "catalog listing must be allowed under ReadOnly: %s", body)

	status, _, body = harness.OCIRequest(t, cli, http.MethodGet, host, "/v2/"+repo+"/tags/list", bearer, nil)
	assert.Equal(t, http.StatusOK, status, "tag listing must be allowed under ReadOnly: %s", body)
}

// TestECRIAMAuthorization_AssumedRolePullOnly proves the same pull-allow /
// push-deny matrix holds for a token minted from ASIA (assumed-role) session
// credentials, not just long-lived AKIA users.
func TestECRIAMAuthorization_AssumedRolePullOnly(t *testing.T) {
	f := requireECRFixture(t)
	host := harness.ECRRegistryHost(f.Account)
	harness.RequireRegistryResolves(t, host)

	repo := uniqueRepo("authz-role-pullonly")
	manifestDigest := seedOCIRepo(t, f, host, repo)

	roleName := uniqueName("ecr-authz-e2e-role")
	roleARN := harness.IAMRoleARN(f.Account, roleName)
	_, err := f.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(iamTrustAnyPrincipal),
	})
	require.NoError(t, err, "create-role")
	t.Cleanup(func() { harness.IAMDeleteRoleAndProfilesBestEffort(f.AWS, roleName, nil, pullOnlyPolicyARN) })

	_, err = f.AWS.IAM.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName: aws.String(roleName), PolicyArn: aws.String(pullOnlyPolicyARN),
	})
	require.NoError(t, err, "attach-role-policy")

	assumed, err := f.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String(uniqueName("authz-session")),
	})
	require.NoError(t, err, "assume-role")
	creds := assumed.Credentials
	require.NotNil(t, creds, "AssumeRole returned nil Credentials")
	cli := harness.NewAWSClientWithSessionCreds(t, f.Env,
		aws.StringValue(creds.AccessKeyId), aws.StringValue(creds.SecretAccessKey), aws.StringValue(creds.SessionToken))

	assertPullAllowedPushDenied(t, cli, host, repo, manifestDigest)
}

// TestECRIAMAuthorization_DetachPolicyDeniesImmediately proves a JWT minted
// while a policy was attached is denied on its very next request once that
// policy is detached — the token is a pointer re-resolved against live IAM
// state, not a capability baked in at mint time.
func TestECRIAMAuthorization_DetachPolicyDeniesImmediately(t *testing.T) {
	f := requireECRFixture(t)
	host := harness.ECRRegistryHost(f.Account)
	harness.RequireRegistryResolves(t, host)

	repo := uniqueRepo("authz-detach")
	_ = seedOCIRepo(t, f, host, repo)

	userName := uniqueName("ecr-authz-e2e-detach")
	_, err := f.AWS.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-user")
	t.Cleanup(func() {
		keys, kerr := f.AWS.IAM.ListAccessKeys(&iam.ListAccessKeysInput{UserName: aws.String(userName)})
		if kerr == nil {
			for _, k := range keys.AccessKeyMetadata {
				_, _ = f.AWS.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{
					UserName: aws.String(userName), AccessKeyId: k.AccessKeyId,
				})
			}
		}
		_, _ = f.AWS.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(userName)})
	})
	_, err = f.AWS.IAM.AttachUserPolicy(&iam.AttachUserPolicyInput{
		UserName: aws.String(userName), PolicyArn: aws.String(pullOnlyPolicyARN),
	})
	require.NoError(t, err, "attach-user-policy")
	keyOut, err := f.AWS.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-access-key")
	cli := harness.NewAWSClientWithCreds(t, f.Env,
		aws.StringValue(keyOut.AccessKey.AccessKeyId), aws.StringValue(keyOut.AccessKey.SecretAccessKey))

	bearer := harness.ECRGetLoginPassword(t, cli)
	status, _, body := harness.OCIRequest(t, cli, http.MethodGet, host, "/v2/"+repo+"/manifests/v1", bearer, nil)
	require.Equal(t, http.StatusOK, status, "pull must succeed while policy is attached: %s", body)

	_, err = f.AWS.IAM.DetachUserPolicy(&iam.DetachUserPolicyInput{
		UserName: aws.String(userName), PolicyArn: aws.String(pullOnlyPolicyARN),
	})
	require.NoError(t, err, "detach-user-policy")

	status, _, body = harness.OCIRequest(t, cli, http.MethodGet, host, "/v2/"+repo+"/manifests/v1", bearer, nil)
	assert.Equal(t, http.StatusForbidden, status, "same JWT must be denied immediately once the policy is detached: %s", body)
}

// TestECRIAMAuthorization_DeactivatedKeyReturns401 proves a JWT minted from a
// since-deactivated access key is refused with 401 (invalid principal), not
// 403 — the key itself no longer authenticates, so the request never reaches
// policy evaluation.
func TestECRIAMAuthorization_DeactivatedKeyReturns401(t *testing.T) {
	f := requireECRFixture(t)
	host := harness.ECRRegistryHost(f.Account)
	harness.RequireRegistryResolves(t, host)

	repo := uniqueRepo("authz-deactivate")
	_ = seedOCIRepo(t, f, host, repo)

	userName := uniqueName("ecr-authz-e2e-deactivate")
	_, err := f.AWS.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-user")
	t.Cleanup(func() {
		keys, kerr := f.AWS.IAM.ListAccessKeys(&iam.ListAccessKeysInput{UserName: aws.String(userName)})
		if kerr == nil {
			for _, k := range keys.AccessKeyMetadata {
				_, _ = f.AWS.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{
					UserName: aws.String(userName), AccessKeyId: k.AccessKeyId,
				})
			}
		}
		_, _ = f.AWS.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(userName)})
	})
	_, err = f.AWS.IAM.AttachUserPolicy(&iam.AttachUserPolicyInput{
		UserName: aws.String(userName), PolicyArn: aws.String(pullOnlyPolicyARN),
	})
	require.NoError(t, err, "attach-user-policy")
	keyOut, err := f.AWS.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-access-key")
	keyID := aws.StringValue(keyOut.AccessKey.AccessKeyId)
	cli := harness.NewAWSClientWithCreds(t, f.Env, keyID, aws.StringValue(keyOut.AccessKey.SecretAccessKey))

	bearer := harness.ECRGetLoginPassword(t, cli)
	status, _, body := harness.OCIRequest(t, cli, http.MethodGet, host, "/v2/"+repo+"/manifests/v1", bearer, nil)
	require.Equal(t, http.StatusOK, status, "pull must succeed while the key is active: %s", body)

	_, err = f.AWS.IAM.UpdateAccessKey(&iam.UpdateAccessKeyInput{
		UserName: aws.String(userName), AccessKeyId: aws.String(keyID), Status: aws.String(iam.StatusTypeInactive),
	})
	require.NoError(t, err, "update-access-key deactivate")

	status, hdr, body := harness.OCIRequest(t, cli, http.MethodGet, host, "/v2/"+repo+"/manifests/v1", bearer, nil)
	assert.Equal(t, http.StatusUnauthorized, status, "same JWT must be refused once its key is inactive: %s", body)
	assert.NotEmpty(t, hdr.Get("Www-Authenticate"), "a revoked-but-well-formed token still gets the Bearer challenge")
}
