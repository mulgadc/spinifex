package handlers_sts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
)

// webIdentityClaims is the RegisteredClaims subset the IRSA flow consumes.
// The wider set of K8s ServiceAccount-token claims (kubernetes.io/*) is
// ignored: STS only needs iss/sub/aud/exp to authorise the role assumption.
type webIdentityClaims struct {
	jwt.RegisteredClaims
}

// AssumeRoleWithWebIdentity exchanges an OIDC ID token (typically a projected
// Kubernetes ServiceAccount token signed by an EKS cluster's per-cluster
// ECDSA-P256 signing key) for short-lived AWS credentials bound to the target
// IAM role.
//
// Verification order (each step fails closed on any error):
//
//  1. Validate input shape.
//  2. Parse JWT with ES256 keyfunc that resolves issuer → registered OIDC
//     provider → cluster JWKS → JWK by kid → *ecdsa.PublicKey.
//  3. Validate `aud` contains `sts.amazonaws.com` (IRSA convention).
//  4. Resolve and look up the target role.
//  5. Evaluate the role's trust policy under web-identity semantics.
//  6. Mint a session credential bound to the role.
//
// Anonymous action — caller identity is the JWT, not SigV4. The gateway
// dispatcher does not gate this action with checkPolicy.
func (s *STSServiceImpl) AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	roleARN := aws.StringValue(input.RoleArn)
	sessionName := aws.StringValue(input.RoleSessionName)
	rawToken := aws.StringValue(input.WebIdentityToken)
	if roleARN == "" || sessionName == "" || rawToken == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if !roleSessionNameRegex.MatchString(sessionName) {
		return nil, errors.New(awserrors.ErrorValidationError)
	}
	if aws.StringValue(input.Policy) != "" || len(input.PolicyArns) > 0 {
		return nil, errors.New(awserrors.ErrorPackedPolicyTooLarge)
	}

	roleAccountID, roleName, err := parseRoleARN(roleARN)
	if err != nil {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	claims := &webIdentityClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"ES256"}),
		jwt.WithExpirationRequired(),
		// Audience is validated explicitly below — the IRSA contract requires
		// sts.amazonaws.com membership, and the jwt/v5 default audience check
		// is a string-equality match on a configurable expected value that
		// does not capture set-membership semantics on multi-aud tokens.
	)
	_, err = parser.ParseWithClaims(rawToken, claims, s.webIdentityKeyFunc(roleAccountID))
	if err != nil {
		slog.Warn("AssumeRoleWithWebIdentity: token verify failed",
			"role_arn", roleARN, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	if claims.Issuer == "" || claims.Subject == "" {
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}
	if !claimAudienceContains(claims.Audience, irsaExpectedAudience) {
		slog.Warn("AssumeRoleWithWebIdentity: aud claim missing sts.amazonaws.com",
			"role_arn", roleARN, "aud", []string(claims.Audience))
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	roleOut, err := s.iamSvc.GetRole(roleAccountID, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		// Cross-account miss → AccessDenied (per the AssumeRole pattern —
		// prevents cross-account role enumeration). Same-account NoSuchEntity
		// surfaces as-is.
		if err.Error() == awserrors.ErrorIAMNoSuchEntity {
			return nil, errors.New(awserrors.ErrorAccessDenied)
		}
		return nil, err
	}
	role := roleOut.Role

	duration := defaultDurationSeconds
	if input.DurationSeconds != nil {
		duration = *input.DurationSeconds
	}
	effectiveMax := aws.Int64Value(role.MaxSessionDuration)
	if effectiveMax == 0 {
		effectiveMax = defaultDurationSeconds
	}
	if effectiveMax > maxDurationSeconds {
		effectiveMax = maxDurationSeconds
	}
	if duration < minDurationSeconds || duration > effectiveMax {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	issuerHostPath := stripIssuerScheme(claims.Issuer)
	federatedARN := handlers_iam.OIDCProviderARN(roleAccountID, issuerHostPath)
	ctx := webIdentityContext{
		federatedPrincipalARN: federatedARN,
		issuer:                claims.Issuer,
		subject:               claims.Subject,
		audience:              []string(claims.Audience),
	}
	if err := evalTrustPolicyForWebIdentity(aws.StringValue(role.AssumeRolePolicyDocument), ctx); err != nil {
		return nil, err
	}

	cred, plainSecret, plainToken, err := s.mintSessionCredential(role, roleAccountID, sessionName, claims.Subject, duration)
	if err != nil {
		return nil, err
	}

	slog.Info("AssumeRoleWithWebIdentity success",
		"role_arn", aws.StringValue(role.Arn),
		"session_name", sessionName,
		"issuer", claims.Issuer,
		"subject", claims.Subject,
		"akid", cred.AccessKeyID,
		"expires_at", cred.ExpiresAt,
	)

	primaryAudience := ""
	if len(claims.Audience) > 0 {
		primaryAudience = claims.Audience[0]
	}

	return &sts.AssumeRoleWithWebIdentityOutput{
		Credentials: &sts.Credentials{
			AccessKeyId:     aws.String(cred.AccessKeyID),
			SecretAccessKey: aws.String(plainSecret),
			SessionToken:    aws.String(plainToken),
			Expiration:      aws.Time(cred.ExpiresAt),
		},
		AssumedRoleUser: &sts.AssumedRoleUser{
			AssumedRoleId: aws.String(cred.AssumedRoleID),
			Arn:           aws.String(cred.AssumedRoleARN),
		},
		SubjectFromWebIdentityToken: aws.String(claims.Subject),
		Provider:                    aws.String(issuerHostPath),
		Audience:                    aws.String(primaryAudience),
		PackedPolicySize:            aws.Int64(0),
	}, nil
}

// webIdentityKeyFunc returns a jwt.Keyfunc bound to the role's account. The
// `iss` claim drives JWKS discovery, but the OIDC-provider registry is read
// from the role-account's IAM bucket (per Q4 — cross-account IRSA against an
// issuer registered in a different account is not supported v1).
//
// Resolution path: iss → ParseEKSIssuerURL → (issuerAccountID, clusterName)
// → registered-provider check in iam-account-{roleAccountID} → FetchClusterJWKS
// from eks-account-{issuerAccountID} → JWK by kid → *ecdsa.PublicKey.
//
// Failures surface as plain errors to the parser; the caller maps them all
// to ErrorInvalidIdentityToken so the token is rejected with a single
// auditable error code regardless of which step failed.
func (s *STSServiceImpl) webIdentityKeyFunc(roleAccountID string) jwt.Keyfunc {
	return func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid header")
		}
		claims, ok := t.Claims.(*webIdentityClaims)
		if !ok {
			return nil, errors.New("unexpected claims type")
		}
		issuer := claims.Issuer
		if issuer == "" {
			return nil, errors.New("missing iss claim")
		}
		issuerAccountID, clusterName, err := ParseEKSIssuerURL(issuer)
		if err != nil {
			return nil, fmt.Errorf("parse issuer: %w", err)
		}
		if err := s.verifyOIDCProviderRegistered(roleAccountID, issuer); err != nil {
			return nil, err
		}
		jwks, err := FetchClusterJWKS(s.js, issuerAccountID, clusterName)
		if err != nil {
			return nil, fmt.Errorf("fetch JWKS: %w", err)
		}
		if jwks == nil {
			return nil, errors.New("cluster JWKS not published")
		}
		jwk := jwks.FindByKID(kid)
		if jwk == nil {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		return jwkToECDSAPublicKey(jwk)
	}
}

// verifyOIDCProviderRegistered looks up the issuer in the role-account's IAM
// OIDC-provider registry. Bucket-not-found is treated identically to
// key-not-found: the account has no providers registered yet, so no token
// from any issuer can succeed. The error message intentionally does not
// distinguish the two — leaking "your bucket exists" leaks account
// activation status to anonymous callers.
func (s *STSServiceImpl) verifyOIDCProviderRegistered(roleAccountID, issuer string) error {
	bucketName := handlers_iam.IAMAccountBucketName(roleAccountID)
	kv, err := s.js.KeyValue(bucketName)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			return errors.New("OIDC provider not registered")
		}
		return fmt.Errorf("open IAM account bucket %s: %w", bucketName, err)
	}
	_, err = kv.Get(handlers_iam.OIDCProviderKey(issuer))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return errors.New("OIDC provider not registered")
		}
		return fmt.Errorf("read OIDC provider: %w", err)
	}
	return nil
}

// jwkToECDSAPublicKey decodes a JWK (RFC 7517) EC entry into an
// *ecdsa.PublicKey. Only ES256 / P-256 is supported v1 — matches the per-
// cluster signing key generated at CreateCluster (eks-v1.md Q8). Any other
// shape fails closed.
func jwkToECDSAPublicKey(jwk *JWK) (*ecdsa.PublicKey, error) {
	if jwk == nil {
		return nil, errors.New("nil JWK")
	}
	if jwk.Kty != "EC" {
		return nil, fmt.Errorf("unsupported kty %q (only EC supported)", jwk.Kty)
	}
	if jwk.Crv != "P-256" {
		return nil, fmt.Errorf("unsupported crv %q (only P-256 supported)", jwk.Crv)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("decode JWK x: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("decode JWK y: %w", err)
	}
	curve := elliptic.P256()
	pub := &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}
	if !curve.IsOnCurve(pub.X, pub.Y) {
		return nil, errors.New("JWK x,y not on P-256 curve")
	}
	return pub, nil
}

// claimAudienceContains is a constant-time membership test on the JWT `aud`
// claim. jwt/v5 already canonicalises string-or-array into ClaimStrings;
// this just iterates.
func claimAudienceContains(aud jwt.ClaimStrings, want string) bool {
	return slices.Contains(aud, want)
}

// stripIssuerScheme drops the `https://` prefix from an issuer URL to produce
// the form AWS uses as the suffix of `arn:aws:iam::{accountID}:oidc-provider/...`.
// Returns the input unchanged if the prefix is absent (defensive — upstream
// ParseEKSIssuerURL already enforces https, but stripping a non-match would
// produce an empty path).
func stripIssuerScheme(issuer string) string {
	return strings.TrimPrefix(issuer, "https://")
}
