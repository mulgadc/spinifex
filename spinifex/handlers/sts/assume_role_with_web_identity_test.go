package handlers_sts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testWebClusterName = "demo-cluster"
	testWebRegion      = "au-mel-1"
	testWebSuffix      = "mulga.local"
)

// webIdentityFixture wraps the cluster-specific crypto + KV state needed to
// drive AssumeRoleWithWebIdentity end-to-end. Each test builds one from the
// shared STSServiceImpl produced by newTestSetup.
type webIdentityFixture struct {
	t            *testing.T
	svc          *STSServiceImpl
	accountID    string
	clusterName  string
	issuer       string
	kid          string
	signingKey   *ecdsa.PrivateKey
	federatedARN string
}

func newWebIdentityFixture(t *testing.T, svc *STSServiceImpl, accountID string) *webIdentityFixture {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	const kid = "test-kid-1"
	jwks := &JWKS{Keys: []JWK{{
		Kty: "EC", Crv: "P-256", Alg: "ES256", Use: "sig", Kid: kid,
		X: base64.RawURLEncoding.EncodeToString(priv.X.Bytes()),
		Y: base64.RawURLEncoding.EncodeToString(priv.Y.Bytes()),
	}}}

	issuer := fmt.Sprintf("https://gw.%s/oidc/eks/%s/%s/%s",
		testWebSuffix, testWebRegion, accountID, testWebClusterName)
	issuerHostPath := strings.TrimPrefix(issuer, "https://")
	federatedARN := handlers_iam.OIDCProviderARN(accountID, issuerHostPath)

	// Publish JWKS to the per-cluster EKS bucket.
	kv, err := handlers_eks.GetOrCreateAccountBucket(svc.js, accountID, 1)
	require.NoError(t, err)
	raw, err := json.Marshal(jwks)
	require.NoError(t, err)
	_, err = kv.Put(handlers_eks.OIDCJWKSKey(testWebClusterName), raw)
	require.NoError(t, err)

	// Register the OIDC provider on the role-account's IAM bucket. Bucket is
	// created on-the-fly here — the IAM CreateOpenIDConnectProvider API lands
	// in Sprint 6e.
	iamKV, err := utils.GetOrCreateKVBucket(svc.js,
		handlers_iam.IAMAccountBucketName(accountID), handlers_iam.KVBucketIAMAccountVersion)
	require.NoError(t, err)
	_, err = iamKV.Put(handlers_iam.OIDCProviderKey(issuer), []byte(`{"registered":true}`))
	require.NoError(t, err)

	return &webIdentityFixture{
		t:            t,
		svc:          svc,
		accountID:    accountID,
		clusterName:  testWebClusterName,
		issuer:       issuer,
		kid:          kid,
		signingKey:   priv,
		federatedARN: federatedARN,
	}
}

// signToken mints a signed ES256 JWT with the supplied claims. Callers tweak
// individual fields (issuer, audience, subject, expiry) to exercise validation
// branches.
func (f *webIdentityFixture) signToken(claims jwt.MapClaims) string {
	f.t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = f.kid
	signed, err := token.SignedString(f.signingKey)
	require.NoError(f.t, err)
	return signed
}

func (f *webIdentityFixture) defaultClaims() jwt.MapClaims {
	now := time.Now().UTC()
	return jwt.MapClaims{
		"iss": f.issuer,
		"sub": "system:serviceaccount:default:my-sa",
		"aud": []string{irsaExpectedAudience},
		"exp": now.Add(15 * time.Minute).Unix(),
		"iat": now.Unix(),
		"nbf": now.Unix(),
	}
}

func webIdentityTrustPolicy(federatedARN, issuer, sub string) string {
	return fmt.Sprintf(`{
        "Version":"2012-10-17",
        "Statement":[{
            "Effect":"Allow",
            "Principal":{"Federated":%q},
            "Action":"sts:AssumeRoleWithWebIdentity",
            "Condition":{"StringEquals":{%q:%q,%q:%q}}
        }]
    }`,
		federatedARN,
		strings.TrimSuffix(issuer, "/")+":sub", sub,
		strings.TrimSuffix(issuer, "/")+":aud", irsaExpectedAudience,
	)
}

func webIdentityTrustPolicyNoCondition(federatedARN string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":%q},"Action":"sts:AssumeRoleWithWebIdentity"}]}`, federatedARN)
}

func createRoleForFixture(t *testing.T, svc *STSServiceImpl, accountID, name, trustPolicy string) *iam.Role {
	t.Helper()
	out, err := svc.iamSvc.CreateRole(accountID, &iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
	})
	require.NoError(t, err)
	return out.Role
}

// ----- Happy path + claim/output round-trip --------------------------------

func TestAssumeRoleWithWebIdentity_HappyPath(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-app",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))

	token := f.signToken(f.defaultClaims())
	out, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("pod-1"),
		WebIdentityToken: aws.String(token),
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.Credentials)

	akid := aws.StringValue(out.Credentials.AccessKeyId)
	assert.True(t, strings.HasPrefix(akid, SessionAccessKeyIDPrefix))
	assert.Equal(t, "system:serviceaccount:default:my-sa", aws.StringValue(out.SubjectFromWebIdentityToken))
	assert.Equal(t, strings.TrimPrefix(f.issuer, "https://"), aws.StringValue(out.Provider))
	assert.Equal(t, irsaExpectedAudience, aws.StringValue(out.Audience))

	stored, err := svc.LookupSessionCredential(akid)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "pod-1", stored.SessionName)
	assert.Equal(t, "system:serviceaccount:default:my-sa", stored.SourceIdentity,
		"sub claim must be persisted as SourceIdentity so audit logs trace back to the pod SA")
}

func TestAssumeRoleWithWebIdentity_NoConditionTrustPolicy(t *testing.T) {
	// A trust policy that names Federated but omits Condition still grants —
	// AWS allows this shape ("trust any pod under this issuer"). The handler
	// must not refuse on missing Condition.
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-open",
		webIdentityTrustPolicyNoCondition(f.federatedARN))

	token := f.signToken(f.defaultClaims())
	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("pod"),
		WebIdentityToken: aws.String(token),
	})
	require.NoError(t, err)
}

// ----- JWT verification failures ------------------------------------------

func TestAssumeRoleWithWebIdentity_TamperedSignature(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-tamper",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))

	token := f.signToken(f.defaultClaims())
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	// Flip a byte in the signature.
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	sigBytes[len(sigBytes)-1] ^= 0xFF
	parts[2] = base64.RawURLEncoding.EncodeToString(sigBytes)
	tampered := strings.Join(parts, ".")

	_, err = svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(tampered),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestAssumeRoleWithWebIdentity_Expired(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-expired",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))

	claims := f.defaultClaims()
	claims["exp"] = time.Now().UTC().Add(-1 * time.Minute).Unix()
	claims["iat"] = time.Now().UTC().Add(-30 * time.Minute).Unix()
	token := f.signToken(claims)

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestAssumeRoleWithWebIdentity_MissingExp(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-no-exp",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))

	claims := f.defaultClaims()
	delete(claims, "exp")
	token := f.signToken(claims)

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestAssumeRoleWithWebIdentity_WrongAudience(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-wrong-aud",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))

	claims := f.defaultClaims()
	claims["aud"] = []string{"not-sts.example.com"}
	token := f.signToken(claims)

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestAssumeRoleWithWebIdentity_UnknownKID(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-bad-kid",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))

	token := jwt.NewWithClaims(jwt.SigningMethodES256, f.defaultClaims())
	token.Header["kid"] = "missing-kid"
	signed, err := token.SignedString(f.signingKey)
	require.NoError(t, err)

	_, err = svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(signed),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestAssumeRoleWithWebIdentity_WrongSigningAlgorithm(t *testing.T) {
	// HS256-signed token must be rejected — the keyfunc only honours ES256
	// and accepting HS256 with a "key" return value would degrade the
	// security model to symmetric.
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-hs256",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, f.defaultClaims())
	token.Header["kid"] = f.kid
	signed, err := token.SignedString([]byte("symmetric-key"))
	require.NoError(t, err)

	_, err = svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(signed),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestAssumeRoleWithWebIdentity_IssuerNotEKSShape(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-bad-iss",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:my-sa"))

	claims := f.defaultClaims()
	claims["iss"] = "https://attacker.example.com/123/c1"
	token := f.signToken(claims)

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

func TestAssumeRoleWithWebIdentity_ProviderNotRegistered(t *testing.T) {
	svc, _ := newTestSetup(t)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	const kid = "unregistered"
	issuer := fmt.Sprintf("https://gw.%s/oidc/eks/%s/%s/%s",
		testWebSuffix, testWebRegion, testCallerAccountID, "unregistered-cluster")
	issuerHostPath := strings.TrimPrefix(issuer, "https://")
	federatedARN := handlers_iam.OIDCProviderARN(testCallerAccountID, issuerHostPath)

	// Publish JWKS — but skip the IAM provider registration.
	kv, err := handlers_eks.GetOrCreateAccountBucket(svc.js, testCallerAccountID, 1)
	require.NoError(t, err)
	jwks := &JWKS{Keys: []JWK{{
		Kty: "EC", Crv: "P-256", Alg: "ES256", Use: "sig", Kid: kid,
		X: base64.RawURLEncoding.EncodeToString(priv.X.Bytes()),
		Y: base64.RawURLEncoding.EncodeToString(priv.Y.Bytes()),
	}}}
	raw, err := json.Marshal(jwks)
	require.NoError(t, err)
	_, err = kv.Put(handlers_eks.OIDCJWKSKey("unregistered-cluster"), raw)
	require.NoError(t, err)

	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-unreg",
		webIdentityTrustPolicyNoCondition(federatedARN))

	now := time.Now().UTC()
	claims := jwt.MapClaims{
		"iss": issuer,
		"sub": "system:serviceaccount:default:my-sa",
		"aud": []string{irsaExpectedAudience},
		"exp": now.Add(15 * time.Minute).Unix(),
		"iat": now.Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(priv)
	require.NoError(t, err)

	_, err = svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(signed),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

// ----- Trust-policy condition checks --------------------------------------

func TestAssumeRoleWithWebIdentity_SubjectMismatch(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-sub",
		webIdentityTrustPolicy(f.federatedARN, f.issuer, "system:serviceaccount:default:expected-sa"))

	claims := f.defaultClaims()
	claims["sub"] = "system:serviceaccount:default:different-sa"
	token := f.signToken(claims)

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssumeRoleWithWebIdentity_FederatedPrincipalMismatch(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	otherARN := handlers_iam.OIDCProviderARN(testCallerAccountID, "oidc.eks.other.example.com/000000000000/other-cluster")
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-other",
		webIdentityTrustPolicyNoCondition(otherARN))

	token := f.signToken(f.defaultClaims())
	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssumeRoleWithWebIdentity_FederatedWildcardDoesNotMatch(t *testing.T) {
	// Principal: "*" must NOT grant a Federated principal — distinguishing
	// authenticated-anyone (AssumeRole) from anonymous-anyone (no analogue
	// for AssumeRoleWithWebIdentity) is load-bearing for IRSA security.
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-wild",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"sts:AssumeRoleWithWebIdentity"}]}`)

	token := f.signToken(f.defaultClaims())
	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssumeRoleWithWebIdentity_ExplicitDenyWinsOverAllow(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	const sub = "system:serviceaccount:default:my-sa"
	policy := fmt.Sprintf(`{
        "Version":"2012-10-17",
        "Statement":[
            {"Effect":"Allow","Principal":{"Federated":%q},"Action":"sts:AssumeRoleWithWebIdentity"},
            {"Effect":"Deny","Principal":{"Federated":%q},"Action":"sts:AssumeRoleWithWebIdentity","Condition":{"StringEquals":{%q:%q}}}
        ]
    }`,
		f.federatedARN, f.federatedARN,
		strings.TrimSuffix(f.issuer, "/")+":sub", sub,
	)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-deny", policy)

	token := f.signToken(f.defaultClaims())
	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

// ----- Input validation ---------------------------------------------------

func TestAssumeRoleWithWebIdentity_RejectsMissingFields(t *testing.T) {
	svc, _ := newTestSetup(t)

	cases := []struct {
		name  string
		input *sts.AssumeRoleWithWebIdentityInput
	}{
		{"nil_input", nil},
		{"missing_role_arn", &sts.AssumeRoleWithWebIdentityInput{
			RoleSessionName: aws.String("x"), WebIdentityToken: aws.String("t"),
		}},
		{"missing_session_name", &sts.AssumeRoleWithWebIdentityInput{
			RoleArn: aws.String("arn:aws:iam::000000000000:role/x"), WebIdentityToken: aws.String("t"),
		}},
		{"missing_token", &sts.AssumeRoleWithWebIdentityInput{
			RoleArn: aws.String("arn:aws:iam::000000000000:role/x"), RoleSessionName: aws.String("x"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.AssumeRoleWithWebIdentity(tc.input)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
		})
	}
}

func TestAssumeRoleWithWebIdentity_RejectsSessionPolicies(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-pol",
		webIdentityTrustPolicyNoCondition(f.federatedARN))
	token := f.signToken(f.defaultClaims())

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
		Policy:           aws.String(`{"Version":"2012-10-17","Statement":[]}`),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorPackedPolicyTooLarge, err.Error())
}

func TestAssumeRoleWithWebIdentity_RejectsBadRoleARN(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	token := f.signToken(f.defaultClaims())

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          aws.String("not-an-arn"),
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationError, err.Error())
}

func TestAssumeRoleWithWebIdentity_RejectsInvalidSessionName(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-name",
		webIdentityTrustPolicyNoCondition(f.federatedARN))
	token := f.signToken(f.defaultClaims())

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("has/slash"),
		WebIdentityToken: aws.String(token),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationError, err.Error())
}

func TestAssumeRoleWithWebIdentity_DurationBounds(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-dur",
		webIdentityTrustPolicyNoCondition(f.federatedARN))
	token := f.signToken(f.defaultClaims())

	_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("sess"),
		WebIdentityToken: aws.String(token),
		DurationSeconds:  aws.Int64(minDurationSeconds - 1),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationError, err.Error())
}

// ----- JWK decoding unit tests --------------------------------------------

func TestJWKToECDSAPublicKey_RoundTrip(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	jwk := &JWK{
		Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(priv.X.Bytes()),
		Y: base64.RawURLEncoding.EncodeToString(priv.Y.Bytes()),
	}
	pub, err := jwkToECDSAPublicKey(jwk)
	require.NoError(t, err)
	assert.True(t, priv.PublicKey.Equal(pub))
}

func TestJWKToECDSAPublicKey_RejectsUnsupportedShapes(t *testing.T) {
	cases := []struct {
		name string
		jwk  *JWK
	}{
		{"nil", nil},
		{"rsa_kty", &JWK{Kty: "RSA", Crv: "P-256", X: "AA", Y: "AA"}},
		{"wrong_curve", &JWK{Kty: "EC", Crv: "P-384", X: "AA", Y: "AA"}},
		{"bad_x_base64", &JWK{Kty: "EC", Crv: "P-256", X: "!!!", Y: "AA"}},
		{"off_curve", &JWK{Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}), Y: base64.RawURLEncoding.EncodeToString([]byte{4, 5, 6})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jwkToECDSAPublicKey(tc.jwk)
			require.Error(t, err)
		})
	}
}
