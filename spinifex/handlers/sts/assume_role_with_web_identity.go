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
	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
)

// webIdentityClaims is the RegisteredClaims subset the IRSA flow consumes (iss/sub/aud/exp).
type webIdentityClaims struct {
	jwt.RegisteredClaims
}

// AssumeRoleWithWebIdentity exchanges an OIDC ID token (typically a K8s ServiceAccount
// token) for short-lived credentials bound to the target IAM role. Validates input,
// verifies the JWT (ES256, registered OIDC provider, aud must contain sts.amazonaws.com),
// evaluates the trust policy, and mints a session. Anonymous — identity is the JWT, not SigV4.
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

	roleAccountID, roleName, err := auth.ParseRoleARN(roleARN)
	if err != nil {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	claims := &webIdentityClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"ES256"}),
		jwt.WithExpirationRequired(),
		// Audience validated explicitly below; jwt/v5 default check is
		// string-equality, which misses set-membership semantics on multi-aud tokens.
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
	if !slices.Contains(claims.Audience, irsaExpectedAudience) {
		slog.Warn("AssumeRoleWithWebIdentity: aud claim missing sts.amazonaws.com",
			"role_arn", roleARN, "aud", []string(claims.Audience))
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	roleOut, err := s.iamSvc.GetRole(roleAccountID, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		// All misses → AccessDenied to prevent cross-account role enumeration.
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
	effectiveMax = min(effectiveMax, maxDurationSeconds)
	if duration < minDurationSeconds || duration > effectiveMax {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	issuerHostPath := strings.TrimPrefix(claims.Issuer, "https://")
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

	env := assumedRoleEnvelope(role, roleAccountID, sessionName, claims.Subject)
	cred, plainSecret, plainToken, err := s.mintSession(env, duration)
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

// webIdentityKeyFunc returns a jwt.Keyfunc that resolves iss → OIDC provider registry
// (role account) → JWKS (issuer account) → JWK by kid → *ecdsa.PublicKey.
// All failures map to ErrorInvalidIdentityToken at the call site.
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

// verifyOIDCProviderRegistered checks the issuer in the role-account's OIDC-provider
// registry. Bucket-not-found and key-not-found return the same error to avoid leaking
// account activation status to anonymous callers.
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

// jwkToECDSAPublicKey decodes a JWK EC entry (RFC 7517) into an *ecdsa.PublicKey.
// Only ES256 / P-256 is supported; any other shape fails closed.
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
