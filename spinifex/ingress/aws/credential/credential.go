// Package credential holds the vocabulary a request authenticator shares with the
// adapter that verifies credentials against the identity stores. It imports no
// domain, so the adapter can live beside IAM and STS without importing the gateway.
package credential

// Request is what the gateway knows about a credential before it is verified.
type Request struct {
	AccessKeyID string
	// SecurityToken is the X-Amz-Security-Token header, empty when absent.
	SecurityToken string
	// SourceIP is the client address for log lines only.
	SourceIP string
}

// Failure is a rejected credential. The verifier logs the cause; the gateway owns
// the response and the rate limiter, so the failure only names what to record.
type Failure struct {
	// Code is the AWS error code to answer with.
	Code string
	// SignatureMismatch marks the verifySig callback rejecting the signature.
	// The gateway holds the signature, so it logs and fingerprints that case.
	SignatureMismatch bool
	// FingerprintKind and FingerprintParts identify the rate-limit entry to
	// record. An empty kind records nothing.
	FingerprintKind  string
	FingerprintParts []string
	// SessionGone marks a session credential whose record has expired or been removed,
	// as opposed to one rejected for any other reason.
	SessionGone bool
	// RoleGone marks a RehydrateRole rejection because the role is deleted or was
	// recreated under a new ID, as opposed to a dependency fault or a bad input.
	RoleGone bool
}

// Authenticated is a credential whose signature has verified. AccountID is exposed
// so the gateway can run its account-active check before Resolve reads anything else.
type Authenticated struct {
	AccountID string
	// Handle is the verifier's own state for Resolve; the gateway never reads it.
	Handle any
}

// Principal is the caller a verified credential resolves to.
type Principal struct {
	Identity          string
	AccountID         string
	PrincipalType     string
	AssumedRoleARN    string
	AssumedRoleID     string
	UnderlyingRoleARN string
	// UserID is aws:userid; UserARN is an IAM user's stored ARN, empty otherwise.
	UserID  string
	UserARN string
	// CallerARN is the caller's canonical ARN. Rehydrate fills it; Resolve leaves it empty.
	CallerARN string
}

// Verifier authenticates a request's credential. The secret never leaves it.
type Verifier interface {
	// Authenticate looks the credential up and passes its secret to verifySig. The
	// secret lookup is the only identity-store read before verifySig succeeds.
	Authenticate(req Request, verifySig func(secret string) error) (Authenticated, *Failure)
	// Resolve runs the checks that must follow a verified signature and the
	// gateway's account-active check: session-principal continuity and the
	// caller's user ID.
	Resolve(req Request, auth Authenticated) (Principal, *Failure)
}

// RoleRehydrator rebuilds an assumed-role caller from the live role alone, for a
// bearer whose session record has expired or been removed. roleID is the immutable
// ID the bearer was minted for and assumedRoleARN names the session; the role must
// still exist under that ID. A Failure with RoleGone set means it does not.
type RoleRehydrator interface {
	RehydrateRole(accountID, roleID, assumedRoleARN string) (Principal, *Failure)
}

// Rehydrator rebuilds the caller an access key id names from current identity
// state, for a bearer that already proved the key once (an ECR registry token).
// It reads no secret and checks no signature. A Failure with Code InternalError
// is a dependency fault; any other Code is an invalid or revoked credential.
// The account-active check is the caller's, as for Resolve.
type Rehydrator interface {
	Rehydrate(accessKeyID, accountID string) (Principal, *Failure)
}
