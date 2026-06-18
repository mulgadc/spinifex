package gateway_ecrauth

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// tokenIssuer is the fixed iss claim on every minted ECR token.
	tokenIssuer = "awsgw"
	// DefaultTokenTTL is the ECR token lifetime (AWS parity: 12h).
	DefaultTokenTTL = 12 * time.Hour
)

// Claims is the ECR token claim set: registered OIDC claims plus the mulga
// principal attributes the bridge reads to scope a request. accountID is the
// authority for /v2 account scoping; accessKeyID is audit-only.
type Claims struct {
	jwt.RegisteredClaims

	AccountID     string `json:"mulga:accountID"`
	PrincipalType string `json:"mulga:principalType,omitempty"`
	AccessKeyID   string `json:"mulga:accessKeyID,omitempty"`
}

// Principal is the SigV4-authenticated caller GetAuthorizationToken mints a
// token for.
type Principal struct {
	AccountID   string
	ARN         string
	Type        string
	AccessKeyID string
}

// Issuer mints ES256 ECR tokens bound to an audience (ecr.{region}.{suffix}).
type Issuer struct {
	key      *SigningKey
	audience string
	ttl      time.Duration
}

// NewIssuer returns an Issuer signing with key for the given audience.
func NewIssuer(key *SigningKey, audience string) *Issuer {
	return &Issuer{key: key, audience: audience, ttl: DefaultTokenTTL}
}

// Mint returns a signed ES256 JWT for p and its expiry. The kid header lets any
// verifier resolve the public key from the shared signing-key set.
func (i *Issuer) Mint(p Principal) (token string, expiresAt time.Time, err error) {
	if i.key == nil {
		return "", time.Time{}, errors.New("ecrauth: issuer has no signing key")
	}
	if p.AccountID == "" {
		return "", time.Time{}, errors.New("ecrauth: mint requires account ID")
	}
	now := time.Now()
	exp := now.Add(i.ttl)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   p.ARN,
			Audience:  jwt.ClaimStrings{i.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.NewString(),
		},
		AccountID:     p.AccountID,
		PrincipalType: p.Type,
		AccessKeyID:   p.AccessKeyID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = i.key.Kid
	signed, err := tok.SignedString(i.key.priv)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign ECR token: %w", err)
	}
	return signed, exp, nil
}

// Verifier validates ECR tokens against the shared kid->public-key set and a
// fixed expected audience.
type Verifier struct {
	keys     map[string]*ecdsa.PublicKey
	audience string
}

// NewVerifier returns a Verifier over keys for the expected audience.
func NewVerifier(keys map[string]*ecdsa.PublicKey, audience string) *Verifier {
	return &Verifier{keys: keys, audience: audience}
}

// Verify parses and validates raw: ES256, unexpired, known kid, matching iss and
// aud, non-empty accountID. It returns the claims on success.
func (v *Verifier) Verify(raw string) (*Claims, error) {
	claims := &Claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"ES256"}),
		jwt.WithExpirationRequired(),
	)
	_, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		pub, ok := v.keys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		return pub, nil
	})
	if err != nil {
		return nil, err
	}
	if claims.Issuer != tokenIssuer {
		return nil, fmt.Errorf("unexpected issuer %q", claims.Issuer)
	}
	if !slices.Contains(claims.Audience, v.audience) {
		return nil, errors.New("audience mismatch")
	}
	if claims.AccountID == "" {
		return nil, errors.New("missing accountID claim")
	}
	return claims, nil
}
