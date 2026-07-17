package gateway_ecrauth

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"slices"
	"sync"
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

// supportedPrincipalTypes are the mulga:principalType claim values a verified
// token may carry. These mirror the gateway package's principalType constants
// (user, assumed-role, root); this package can't import gateway (it would
// cycle), so the strings are pinned here and by the gateway package's own
// constants — a rename on either side is caught by ecrauth_test.go and by
// the gateway ECR principal tests exercising real Mint/Verify round-trips.
var supportedPrincipalTypes = map[string]bool{
	"user":         true,
	"assumed-role": true,
	"root":         true,
}

// SupportedPrincipalType reports whether t is a principalType claim value
// Verify accepts. Exported so callers minting a token (or tests) can validate
// a type before it round-trips through Mint/Verify.
func SupportedPrincipalType(t string) bool {
	return supportedPrincipalTypes[t]
}

// Claims is the ECR token claim set: registered OIDC claims plus the mulga
// principal attributes the bridge reads to scope a request. The token is a
// signed pointer, not a permission grant: accountID scopes /v2 account
// access, and accessKeyID is the lookup key the gateway uses to rehydrate the
// current IAM/STS record on every request, so a revoked key, session, user,
// role, or policy takes effect on the very next request even against an
// already-issued 12h token. Verify requires all three of these plus Subject
// to be non-empty and PrincipalType to be a supported value.
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
// The active signing key is swapped under mu by the rotation scheduler while
// Mint runs concurrently.
type Issuer struct {
	mu       sync.RWMutex
	key      *SigningKey
	audience string
	ttl      time.Duration
}

// NewIssuer returns an Issuer signing with key for the given audience.
func NewIssuer(key *SigningKey, audience string) *Issuer {
	return &Issuer{key: key, audience: audience, ttl: DefaultTokenTTL}
}

// SetActiveKey swaps the signing key used by subsequent Mint calls. The rotator
// calls it after minting a fresh key.
func (i *Issuer) SetActiveKey(key *SigningKey) {
	i.mu.Lock()
	i.key = key
	i.mu.Unlock()
}

// ActiveKid returns the kid of the current signing key, or "" if none.
func (i *Issuer) ActiveKid() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.key == nil {
		return ""
	}
	return i.key.Kid
}

// Mint returns a signed ES256 JWT for p and its expiry. The kid header lets any
// verifier resolve the public key from the shared signing-key set.
func (i *Issuer) Mint(p Principal) (token string, expiresAt time.Time, err error) {
	i.mu.RLock()
	key := i.key
	i.mu.RUnlock()
	if key == nil {
		return "", time.Time{}, errors.New("ecrauth: issuer has no signing key")
	}
	if p.AccountID == "" {
		return "", time.Time{}, errors.New("ecrauth: mint requires account ID")
	}
	// A registry token is a signed pointer to an IAM/STS record: every field
	// the verifier will later demand back must be present at mint time, or
	// the token would verify into an unusable/ambiguous identity.
	if p.ARN == "" {
		return "", time.Time{}, errors.New("ecrauth: mint requires principal ARN")
	}
	if p.AccessKeyID == "" {
		return "", time.Time{}, errors.New("ecrauth: mint requires access key ID")
	}
	if !SupportedPrincipalType(p.Type) {
		return "", time.Time{}, fmt.Errorf("ecrauth: mint requires a supported principal type, got %q", p.Type)
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
	tok.Header["kid"] = key.Kid
	signed, err := tok.SignedString(key.priv)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign ECR token: %w", err)
	}
	return signed, exp, nil
}

// Verifier validates ECR tokens against the shared kid->public-key set and a
// fixed expected audience. The key set is swapped under mu by the rotation
// scheduler while Verify runs concurrently.
type Verifier struct {
	mu       sync.RWMutex
	keys     map[string]*ecdsa.PublicKey
	audience string
}

// NewVerifier returns a Verifier over keys for the expected audience.
func NewVerifier(keys map[string]*ecdsa.PublicKey, audience string) *Verifier {
	return &Verifier{keys: keys, audience: audience}
}

// SetKeys swaps the kid->public-key verification set. The rotator calls it after
// each cycle so a newly minted key is accepted and pruned keys are dropped.
func (v *Verifier) SetKeys(keys map[string]*ecdsa.PublicKey) {
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
}

// publicKey resolves a kid under the read lock.
func (v *Verifier) publicKey(kid string) (*ecdsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	pub, ok := v.keys[kid]
	return pub, ok
}

// Verify parses and validates raw: ES256, unexpired, known kid, matching iss
// and aud, and a complete identity pointer — non-empty subject, accountID,
// accessKeyID, and a supported principalType. The gateway resolves the
// returned claims against current IAM/STS state before trusting them; Verify
// only proves the token was signed by this issuer and names a well-formed
// identity to look up.
func (v *Verifier) Verify(raw string) (*Claims, error) {
	claims := &Claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"ES256"}),
		jwt.WithExpirationRequired(),
	)
	_, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		pub, ok := v.publicKey(kid)
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
	if claims.Subject == "" {
		return nil, errors.New("missing subject claim")
	}
	if claims.AccountID == "" {
		return nil, errors.New("missing accountID claim")
	}
	if claims.AccessKeyID == "" {
		return nil, errors.New("missing accessKeyID claim")
	}
	if !SupportedPrincipalType(claims.PrincipalType) {
		return nil, fmt.Errorf("unsupported principalType %q", claims.PrincipalType)
	}
	return claims, nil
}
