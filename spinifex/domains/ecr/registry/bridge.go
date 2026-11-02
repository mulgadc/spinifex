package ecrregistry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/policy"
)

// TokenVerifier verifies a registry token's signature, expiry, issuer and audience.
type TokenVerifier interface {
	Verify(raw string) (*ecrauth.Claims, error)
}

// TokenIssuer mints a registry token.
type TokenIssuer interface {
	Mint(p ecrauth.Principal) (token string, expiresAt time.Time, err error)
}

// Principal is the caller a registry token resolves to, as current identity state
// reports it. The fields mirror what authorization needs to build its condition keys.
type Principal struct {
	Identity          string
	AccountID         string
	PrincipalType     string
	AssumedRoleARN    string
	AssumedRoleID     string
	UnderlyingRoleARN string
	UserID            string
	UserARN           string
	// CallerARN is the caller's canonical ARN, which a token's subject must equal.
	CallerARN string
}

// ErrInvalidCredential marks an identity that no longer resolves: revoked, deleted or
// expired. It answers 401. ErrUnavailable marks a dependency fault and answers 503, so
// an outage is never read as a denial or an allow.
var (
	ErrInvalidCredential = errors.New("invalid credential")
	ErrUnavailable       = errors.New("identity dependency unavailable")
)

// ErrSessionGone marks a Rehydrate failure because the access key's session record
// has expired or been removed. ErrRoleGone marks a RehydrateRole failure because the
// role is deleted or was recreated under a new ID.
var (
	ErrSessionGone = errors.New("session expired or gone")
	ErrRoleGone    = errors.New("role gone")
)

// RoleRehydrator rebuilds an assumed-role principal from the live role named by the
// token's role ID, once the session record behind it is gone.
type RoleRehydrator interface {
	RehydrateRole(accountID, roleID, assumedRoleARN string) (Principal, error)
}

// Rehydrator rebuilds the principal behind an access key from current identity state.
// A failure wraps ErrInvalidCredential or ErrUnavailable.
type Rehydrator interface {
	Rehydrate(accessKeyID, accountID string) (Principal, error)
}

// Authorizer evaluates one IAM action against resources for a principal. A dependency
// fault wraps ErrUnavailable; any other error is a denial.
type Authorizer interface {
	Authorize(r *http.Request, p Principal, iamAction string, resources []string) error
}

// Admission reports whether an account may proceed. An error means its status could
// not be read.
type Admission interface {
	Active(accountID, logIP string) (bool, error)
}

// BridgeConfig is what a Bridge needs. Everything but Registry is required.
type BridgeConfig struct {
	// Registry serves an authorized request. Nil answers 501.
	Registry http.Handler
	// Verifier and Issuer are the registry token pair.
	Verifier TokenVerifier
	Issuer   TokenIssuer
	// Rehydrator rebuilds the token's principal from current identity state.
	Rehydrator Rehydrator
	// RoleRehydrator is optional: without it an expired session is a 401.
	RoleRehydrator RoleRehydrator
	Authorizer     Authorizer
	Admission      Admission
	// Target reports the account and region the request's registry host names.
	Target func(r *http.Request) (account, region string, ok bool)
	// ClientIP reports the client address, for logging.
	ClientIP func(r *http.Request) string
	// Region is the region repository ARNs are built in.
	Region string
}

// Bridge serves the OCI /v2 surface: token authentication, principal rehydration,
// per-operation authorization and the token exchange endpoint. It is not SigV4 and
// is mounted outside the AWS seam.
type Bridge struct {
	registry   http.Handler
	verifier   TokenVerifier
	issuer     TokenIssuer
	rehydrator Rehydrator
	// roleRehydrator is nil when no role fallback is configured.
	roleRehydrator RoleRehydrator
	authorizer     Authorizer
	admission      Admission
	target         func(r *http.Request) (account, region string, ok bool)
	clientIP       func(r *http.Request) string
	region         string
}

// NewBridge returns a Bridge. A missing verifier, issuer, rehydrator, authorizer or
// admission is a composition fault, so it is refused rather than served open.
func NewBridge(cfg BridgeConfig) (*Bridge, error) {
	switch {
	case cfg.Verifier == nil:
		return nil, errors.New("registry bridge: token verifier is required")
	case cfg.Issuer == nil:
		return nil, errors.New("registry bridge: token issuer is required")
	case cfg.Rehydrator == nil:
		return nil, errors.New("registry bridge: principal rehydrator is required")
	case cfg.Authorizer == nil:
		return nil, errors.New("registry bridge: authorizer is required")
	case cfg.Admission == nil:
		return nil, errors.New("registry bridge: account admission is required")
	case cfg.Target == nil:
		return nil, errors.New("registry bridge: host target resolver is required")
	case cfg.ClientIP == nil:
		return nil, errors.New("registry bridge: client address resolver is required")
	}
	return &Bridge{
		registry:       cfg.Registry,
		verifier:       cfg.Verifier,
		issuer:         cfg.Issuer,
		rehydrator:     cfg.Rehydrator,
		roleRehydrator: cfg.RoleRehydrator,
		authorizer:     cfg.Authorizer,
		admission:      cfg.Admission,
		target:         cfg.Target,
		clientIP:       cfg.ClientIP,
		region:         cfg.Region,
	}, nil
}

// Handler serves the /v2 subtree. It expects the full request path, so mount it at
// /v2 without stripping the prefix.
func (b *Bridge) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v2/token" {
			b.serveToken(w, r)
			return
		}
		b.serveRegistry(w, r)
	})
}

// serveRegistry runs authentication, then operation authorization, then the registry.
func (b *Bridge) serveRegistry(w http.ResponseWriter, r *http.Request) {
	claims, principal, ok := b.authenticate(w, r, true)
	if !ok {
		return
	}
	r = r.WithContext(WithAuthAccount(r.Context(), claims.AccountID))

	op, ok := ClassifyOperation(r.Method, r.URL.Path, r.URL.Query())
	if !ok {
		// Registry itself would 404 this path/method combination; refusing it here
		// too means an operation added to Registry without a matching classifier
		// case can never dispatch unauthorized.
		WriteError(w, http.StatusNotFound, "NAME_UNKNOWN", "unrecognized registry operation")
		return
	}

	for _, req := range op.Requirements {
		resource, err := resourceARN(b.region, principal.AccountID, req.Scope, op)
		if err != nil {
			WriteError(w, http.StatusServiceUnavailable, "UNKNOWN", "authorization unavailable")
			return
		}
		if err := b.authorizer.Authorize(r, principal, policy.IAMAction("ecr", req.Action), []string{resource}); err != nil {
			if errors.Is(err, ErrUnavailable) {
				// IAM/STS/NATS dependency failure: fail closed, never dispatch.
				WriteError(w, http.StatusServiceUnavailable, "UNKNOWN", "authorization unavailable")
				return
			}
			WriteError(w, http.StatusForbidden, "DENIED", "insufficient permissions")
			return
		}
	}

	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/v2" || r.URL.Path == "/v2/"):
		APIVersion(w, r)
	case b.registry != nil:
		b.registry.ServeHTTP(w, r)
	default:
		NotImplemented(w, r)
	}
}

// authenticate verifies the request's registry token and rehydrates its principal
// against current identity state, so a revoked key, session, user, role or account
// takes effect on this request even though the token is still valid and unexpired.
// It writes the failure response and reports false when the request must stop.
// challenge selects the 401 Bearer challenge; the token endpoint is itself the
// challenge target, so it answers a bare 401.
func (b *Bridge) authenticate(w http.ResponseWriter, r *http.Request, challenge bool) (*ecrauth.Claims, Principal, bool) {
	deny := func(message string) {
		if challenge {
			writeChallenge(w, r)
			return
		}
		WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", message)
	}

	authz := r.Header.Values("Authorization")
	if len(authz) > 1 {
		WriteError(w, http.StatusBadRequest, "UNAUTHORIZED", "multiple Authorization headers")
		return nil, Principal{}, false
	}
	raw := ""
	if len(authz) == 1 {
		raw = authz[0]
	}
	token, ok := extractToken(raw)
	if !ok {
		deny("authentication required")
		return nil, Principal{}, false
	}
	claims, err := b.verifier.Verify(token)
	if err != nil {
		slog.Debug("ECR registry: token verify failed", "err", err)
		deny("invalid credentials")
		return nil, Principal{}, false
	}

	// A token is account-scoped, so it must match the account in the registry host it
	// is presented against.
	if target, _, ok := b.target(r); ok && target != "" && target != claims.AccountID {
		WriteError(w, http.StatusForbidden, "DENIED", "token account does not match registry host")
		return nil, Principal{}, false
	}

	principal, err := b.rehydrate(r, claims)
	if err != nil {
		if errors.Is(err, ErrRoleGone) {
			// AWS answers a token whose role is deleted with 403 and no challenge, as a
			// retry cannot help.
			slog.Warn("ECR registry: token's role is gone", "accountID", claims.AccountID, "roleID", claims.RoleID)
			WriteError(w, http.StatusForbidden, "DENIED", "The security token included in the request is invalid.")
			return nil, Principal{}, false
		}
		if _, ok := errors.AsType[*dependencyError](err); ok {
			slog.Error("ECR registry: principal rehydration dependency failure", "err", err)
			WriteError(w, http.StatusServiceUnavailable, "UNKNOWN", "authorization unavailable")
			return nil, Principal{}, false
		}
		slog.Warn("ECR registry: principal rehydration rejected token", "err", err)
		deny("invalid credentials")
		return nil, Principal{}, false
	}
	return claims, principal, true
}

// dependencyError marks a rehydration failure caused by an outage rather than by an
// invalid identity, so the response is 503 and never an allow.
type dependencyError struct{ err error }

func (e *dependencyError) Error() string { return e.err.Error() }
func (e *dependencyError) Unwrap() error { return e.err }

// rehydrate rebuilds the token's principal and cross-checks the claims against it. It
// never trusts claims.PrincipalType or claims.Subject on their own: both must match the
// freshly resolved record, so a forged or stale claim cannot widen access.
func (b *Bridge) rehydrate(r *http.Request, claims *ecrauth.Claims) (Principal, error) {
	p, err := b.rehydrator.Rehydrate(claims.AccessKeyID, claims.AccountID)
	if err != nil && errors.Is(err, ErrSessionGone) && claims.RoleID != "" && b.roleRehydrator != nil {
		// AWS keeps the token valid past its session and judges the role as it is now,
		// so the principal comes from the live role and then goes through the same
		// admission and authorization as any other.
		p, err = b.roleRehydrator.RehydrateRole(claims.AccountID, claims.RoleID, claims.Subject)
	}
	if err != nil {
		if errors.Is(err, ErrRoleGone) {
			return Principal{}, fmt.Errorf("rehydrate role: %w", err)
		}
		if errors.Is(err, ErrUnavailable) {
			return Principal{}, &dependencyError{fmt.Errorf("rehydrate: %w", err)}
		}
		return Principal{}, fmt.Errorf("rehydrate: %w", err)
	}

	// An account suspended or being torn down stops working at once, not at the
	// token's natural expiry.
	active, err := b.admission.Active(p.AccountID, b.clientIP(r))
	if err != nil {
		return Principal{}, &dependencyError{fmt.Errorf("account status: %w", err)}
	}
	if !active {
		return Principal{}, fmt.Errorf("account %q is not active", p.AccountID)
	}

	if claims.PrincipalType != p.PrincipalType {
		return Principal{}, fmt.Errorf("principalType claim %q does not match resolved principal", claims.PrincipalType)
	}
	if claims.Subject != p.CallerARN {
		return Principal{}, errors.New("subject claim does not match canonical principal ARN")
	}
	return p, nil
}

// ociTokenResponse is the Docker Registry v2 token-endpoint body. token and
// access_token carry the same JWT for compatibility across client versions.
type ociTokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// serveToken implements the OCI Bearer token endpoint the 401 challenge advertises. The
// caller presents its existing credential; this verifies it, rehydrates the principal
// and re-mints a fresh full-TTL token from that record, never from the presented claims,
// so a revoked identity is not refreshed. Re-minting is the refresh path: a client near
// expiry obtains a new token without re-running SigV4.
func (b *Bridge) serveToken(w http.ResponseWriter, r *http.Request) {
	claims, principal, ok := b.authenticate(w, r, false)
	if !ok {
		return
	}

	fresh, expiresAt, err := b.issuer.Mint(ecrauth.Principal{
		AccountID:   principal.AccountID,
		ARN:         principal.CallerARN,
		Type:        principal.PrincipalType,
		AccessKeyID: claims.AccessKeyID,
		RoleID:      refreshedRoleID(claims, principal),
	})
	if err != nil {
		slog.Error("ECR token endpoint: mint failed", "err", err)
		WriteError(w, http.StatusInternalServerError, "SERVER_ERROR", "token mint failed")
		return
	}

	body, err := json.Marshal(ociTokenResponse{
		Token:       fresh,
		AccessToken: fresh,
		ExpiresIn:   int(time.Until(expiresAt).Seconds()),
		IssuedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		slog.Error("ECR token endpoint: marshal failed", "err", err)
		WriteError(w, http.StatusInternalServerError, "SERVER_ERROR", "response encode failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("ECR token endpoint: write failed", "err", err)
	}
}

// refreshedRoleID is the role ID a re-minted token carries: the presented token's, or for
// a role session the one in the resolved principal's "<roleID>:<session>" ID, so a refresh
// never drops the claim that lets the token outlive its session.
func refreshedRoleID(claims *ecrauth.Claims, p Principal) string {
	if claims.RoleID != "" {
		return claims.RoleID
	}
	if p.PrincipalType != ecrauth.PrincipalTypeAssumedRole {
		return ""
	}
	roleID, _, found := strings.Cut(p.AssumedRoleID, ":")
	if !found {
		return ""
	}
	return roleID
}

// extractToken pulls the JWT from a single Authorization header value, accepting
// "Bearer <jwt>" or "Basic base64(AWS:<jwt>)". ok is false for any other scheme,
// malformed value, or empty token. SigV4 is not accepted on /v2.
func extractToken(authz string) (string, bool) {
	scheme, rest, found := strings.Cut(authz, " ")
	if !found {
		return "", false
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		rest = strings.TrimSpace(rest)
		return rest, rest != ""
	case strings.EqualFold(scheme, "Basic"):
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return "", false
		}
		user, pass, found := strings.Cut(string(decoded), ":")
		if !found || user != "AWS" || pass == "" {
			return "", false
		}
		return pass, true
	default:
		return "", false
	}
}

// writeChallenge emits the 401 Bearer challenge advertising the /v2/token endpoint.
// OCI clients (docker, crane, skopeo) fetch a token from the realm using their stored
// AWS:<jwt> credential, then replay it as Bearer. The realm and service use the
// request Host so the client negotiates against the address it actually dialed.
func writeChallenge(w http.ResponseWriter, r *http.Request) {
	realm := "https://" + r.Host + "/v2/token"
	w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`",service="`+r.Host+`"`)
	WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

// resourceARN builds the repository ARN an ActionRequirement's scope resolves to, from
// the configured region and the token's account.
func resourceARN(region, accountID string, scope ResourceScope, op ClassifiedOperation) (string, error) {
	switch scope {
	case ScopeAccountWildcard:
		return fmt.Sprintf("arn:aws:ecr:%s:%s:repository/*", region, accountID), nil
	case ScopeDestination:
		return fmt.Sprintf("arn:aws:ecr:%s:%s:repository/%s", region, accountID, op.Repo), nil
	case ScopeSource:
		return fmt.Sprintf("arn:aws:ecr:%s:%s:repository/%s", region, accountID, op.Source), nil
	default:
		return "", fmt.Errorf("unhandled ECR resource scope %d", scope)
	}
}
