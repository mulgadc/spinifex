package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/golang-jwt/jwt/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// The TestCurrentBehaviour_ tests in this file pin today's public OIDC discovery
// and JWKS endpoint outcomes, and the wire rendering of a web-identity JWKS
// lookup failure. They describe current behaviour, not the target design.

const (
	cbDiscPath = "/oidc/eks/ap-southeast-2/" + testDiscAccount + "/" + testDiscCluster + "/.well-known/openid-configuration"
	cbKeysPath = "/oidc/eks/ap-southeast-2/" + testDiscAccount + "/" + testDiscCluster + "/keys"
)

type discoveryEnv struct {
	nc *nats.Conn
	kv jetstream.KeyValue
	h  http.Handler
}

func newDiscoveryEnv(t *testing.T) *discoveryEnv {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	kv, err := js.CreateOrUpdateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "eks-account-" + testDiscAccount})
	require.NoError(t, err)
	gw := &GatewayConfig{DisableLogging: true, NATSConn: nc, IAMService: allowAllIAMService()}
	return &discoveryEnv{nc: nc, kv: kv, h: gw.SetupRoutes()}
}

func (e *discoveryEnv) putMeta(t *testing.T, status handlers_eks.ClusterStatus, issuer string) {
	t.Helper()
	require.NoError(t, handlers_eks.PutClusterMeta(t.Context(), e.kv,
		&handlers_eks.ClusterMeta{Name: testDiscCluster, Status: status, OIDCIssuer: issuer}))
}

func (e *discoveryEnv) putJWKS(t *testing.T, raw string) {
	t.Helper()
	_, err := e.kv.Put(t.Context(), "clusters/"+testDiscCluster+"/oidc-jwks.json", []byte(raw))
	require.NoError(t, err)
}

func getOIDC(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The JWKS endpoint serves the stored bytes verbatim without validating them.
func TestCurrentBehaviour_OIDCJWKSServesMalformedValueVerbatim(t *testing.T) {
	e := newDiscoveryEnv(t)
	e.putJWKS(t, `not-json`)

	rec := getOIDC(t, e.h, cbKeysPath)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "not-json", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=300", rec.Header().Get("Cache-Control"))
}

// Discovery answers from the cluster record alone; JWKS answers from the JWKS
// key alone. Neither consults the cluster status.
func TestCurrentBehaviour_OIDCDiscoveryOutcomesByStoredState(t *testing.T) {
	const jwks = `{"keys":[{"kty":"EC","kid":"abc"}]}`
	for _, tc := range []struct {
		name               string
		status             handlers_eks.ClusterStatus
		issuer             string
		withMeta, withJWKS bool
		wantDisc, wantKeys int
	}{
		{name: "active", status: handlers_eks.ClusterStatusActive, issuer: testDiscIssuer, withMeta: true, withJWKS: true, wantDisc: 200, wantKeys: 200},
		{name: "deleting", status: handlers_eks.ClusterStatusDeleting, issuer: testDiscIssuer, withMeta: true, withJWKS: true, wantDisc: 200, wantKeys: 200},
		{name: "failed", status: handlers_eks.ClusterStatusFailed, issuer: testDiscIssuer, withMeta: true, withJWKS: true, wantDisc: 200, wantKeys: 200},
		{name: "meta_without_issuer", status: handlers_eks.ClusterStatusCreating, withMeta: true, withJWKS: true, wantDisc: 404, wantKeys: 200},
		{name: "jwks_absent", status: handlers_eks.ClusterStatusActive, issuer: testDiscIssuer, withMeta: true, wantDisc: 200, wantKeys: 404},
		{name: "jwks_without_meta", withJWKS: true, wantDisc: 404, wantKeys: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDiscoveryEnv(t)
			if tc.withMeta {
				e.putMeta(t, tc.status, tc.issuer)
			}
			if tc.withJWKS {
				e.putJWKS(t, jwks)
			}
			disc := getOIDC(t, e.h, cbDiscPath)
			keys := getOIDC(t, e.h, cbKeysPath)
			assert.Equal(t, tc.wantDisc, disc.Code, "discovery body=%s", disc.Body.String())
			assert.Equal(t, tc.wantKeys, keys.Code, "keys body=%s", keys.Body.String())
			if tc.wantDisc == 200 {
				assert.Equal(t, "public, max-age=300", disc.Header().Get("Cache-Control"))
			}
		})
	}
}

// Cluster deleted: after the EKS teardown sweeps clusters/{name}/, both
// endpoints return the stock 404 body.
func TestCurrentBehaviour_OIDCDiscoveryAfterClusterPrefixDeleted(t *testing.T) {
	e := newDiscoveryEnv(t)
	e.putMeta(t, handlers_eks.ClusterStatusDeleting, testDiscIssuer)
	e.putJWKS(t, `{"keys":[{"kty":"EC","kid":"abc"}]}`)
	require.NoError(t, handlers_eks.DeleteClusterPrefix(t.Context(), e.kv, testDiscCluster))

	for _, p := range []string{cbDiscPath, cbKeysPath} {
		rec := getOIDC(t, e.h, p)
		assert.Equal(t, http.StatusNotFound, rec.Code, p)
		assert.Equal(t, "404 page not found\n", rec.Body.String(), p)
	}
}

// KV unavailable (closed NATS connection or none wired) is a 500 with a
// plain-text body on both endpoints; an absent account bucket is a 404.
func TestCurrentBehaviour_OIDCDiscoveryKVUnavailable(t *testing.T) {
	t.Run("closed_connection", func(t *testing.T) {
		e := newDiscoveryEnv(t)
		e.putMeta(t, handlers_eks.ClusterStatusActive, testDiscIssuer)
		e.putJWKS(t, `{"keys":[]}`)
		e.nc.Close()
		for _, p := range []string{cbDiscPath, cbKeysPath} {
			rec := getOIDC(t, e.h, p)
			assert.Equal(t, http.StatusInternalServerError, rec.Code, p)
			assert.Equal(t, "internal error\n", rec.Body.String(), p)
		}
	})
	t.Run("no_connection", func(t *testing.T) {
		h := (&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}).SetupRoutes()
		for _, p := range []string{cbDiscPath, cbKeysPath} {
			rec := getOIDC(t, h, p)
			assert.Equal(t, http.StatusInternalServerError, rec.Code, p)
			assert.Equal(t, "internal error\n", rec.Body.String(), p)
		}
	})
	t.Run("account_bucket_missing", func(t *testing.T) {
		e := newDiscoveryEnv(t)
		for _, p := range []string{
			"/oidc/eks/ap-southeast-2/999999999999/" + testDiscCluster + "/.well-known/openid-configuration",
			"/oidc/eks/ap-southeast-2/999999999999/" + testDiscCluster + "/keys",
		} {
			rec := getOIDC(t, e.h, p)
			assert.Equal(t, http.StatusNotFound, rec.Code, p)
			assert.Equal(t, "404 page not found\n", rec.Body.String(), p)
		}
	})
}

// An unsigned AssumeRoleWithWebIdentity whose issuer has no published JWKS
// reaches the real STS service and is rendered as a 400 InvalidIdentityToken.
func TestCurrentBehaviour_WebIdentityJWKSMissingWireResponse(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	iamSvc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)
	stsSvc, err := handlers_sts.NewSTSServiceImpl(t.Context(), nc, iamSvc, masterKey, handlers_eks.NewOIDCJWKSReader(js))
	require.NoError(t, err)

	issuer := testDiscIssuer
	iamKV, err := kvutil.GetOrCreateBucket(t.Context(), js,
		iamdomain.IAMAccountBucketName(testDiscAccount), iamdomain.KVBucketIAMAccountVersion)
	require.NoError(t, err)
	_, err = iamKV.Put(t.Context(), iamdomain.OIDCProviderKey(issuer), []byte(`{"registered":true}`))
	require.NoError(t, err)
	federated := iamdomain.OIDCProviderARN(testDiscAccount, strings.TrimPrefix(issuer, "https://"))
	role, err := iamSvc.CreateRole(testDiscAccount, &iam.CreateRoleInput{
		RoleName: aws.String("irsa-wire"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"` +
			federated + `"},"Action":"sts:AssumeRoleWithWebIdentity"}]}`),
	})
	require.NoError(t, err)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": issuer, "sub": "system:serviceaccount:default:sa", "aud": []string{"sts.amazonaws.com"},
		"exp": now.Add(10 * time.Minute).Unix(), "iat": now.Unix(),
	})
	tok.Header["kid"] = "absent"
	signed, err := tok.SignedString(priv)
	require.NoError(t, err)

	gw := &GatewayConfig{DisableLogging: true, NATSConn: nc, IAMService: iamSvc, STSService: stsSvc}
	form := url.Values{
		"Action": {"AssumeRoleWithWebIdentity"}, "Version": {"2011-06-15"},
		"RoleArn": {aws.StringValue(role.Role.Arn)}, "RoleSessionName": {"s1"}, "WebIdentityToken": {signed},
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	gw.SetupRoutes().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "<Code>InvalidIdentityToken</Code>")
	assert.Contains(t, body, "The web identity token that was passed is invalid or expired.")
	assert.NotContains(t, body, "AssumeRoleWithWebIdentityResult")
}
