package handlers_sts

import (
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// A session credential names its principal, and a name is not an identity: an
// administrator who deletes an IAM user or role and recreates it under the same
// name leaves every unexpired session for the original resolving to the
// replacement. Binding the session to the principal's immutable ID at mint and
// re-checking it against the live record at every door that rehydrates a session
// is what makes the two tell apart.
var (
	// ErrSessionPrincipalGone reports that the principal no longer exists.
	ErrSessionPrincipalGone = errors.New("session principal no longer exists")

	// ErrSessionPrincipalReplaced reports that a principal of that name exists but
	// carries a different immutable ID than the session was minted for.
	ErrSessionPrincipalReplaced = errors.New("session principal was replaced since the session was minted")

	// ErrSessionPrincipalLegacy reports that the record holds no immutable ID to
	// check. Rejected rather than waved through: accepting it preserves exactly
	// the gap the check closes, and there is nothing honest to backfill from.
	ErrSessionPrincipalLegacy = errors.New("session record carries no immutable principal ID")
)

// IsSessionPrincipalVerdict reports whether err is one of the three continuity
// verdicts rather than a dependency fault. Doors answer a token error for a
// verdict and InternalError for anything else.
func IsSessionPrincipalVerdict(err error) bool {
	return errors.Is(err, ErrSessionPrincipalGone) ||
		errors.Is(err, ErrSessionPrincipalReplaced) ||
		errors.Is(err, ErrSessionPrincipalLegacy)
}

// SessionPrincipal is the live IAM record a session credential still resolves to.
type SessionPrincipal struct {
	UserID   string // user sessions
	RoleID   string // role sessions
	RoleName string
	RoleARN  string
}

// VerifySessionPrincipal re-resolves the principal a session was minted for and
// reports whether it is still the same one. The three verdicts above all fail
// closed; an IAM dependency fault wraps through unwrapped, so a caller can tell
// a fault from a verdict and answer InternalError rather than a token error.
func (s *STSServiceImpl) VerifySessionPrincipal(cred *SessionCredential) (*SessionPrincipal, error) {
	if cred == nil {
		return nil, errors.New("nil session credential")
	}
	if cred.PrincipalType == principalTypeUser {
		return s.verifySessionUser(cred)
	}
	// "assumed-role", or empty for a record predating PrincipalType.
	return s.verifySessionRole(cred)
}

// verifySessionUser compares the live user's UserId against the one stored at
// mint. There is no ARN comparison to make: the session persists only the user
// name, and no IAM operation can rename or re-path a user.
func (s *STSServiceImpl) verifySessionUser(cred *SessionCredential) (*SessionPrincipal, error) {
	if cred.UserID == "" {
		return nil, ErrSessionPrincipalLegacy
	}

	out, err := s.iamSvc.GetUser(cred.AccountID, &iam.GetUserInput{UserName: aws.String(cred.SessionName)})
	if err != nil {
		if awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity) {
			return nil, ErrSessionPrincipalGone
		}
		return nil, fmt.Errorf("resolve session user: %w", err)
	}
	if out == nil || out.User == nil {
		return nil, ErrSessionPrincipalGone
	}

	liveUserID := aws.StringValue(out.User.UserId)
	if liveUserID != cred.UserID {
		return nil, ErrSessionPrincipalReplaced
	}
	return &SessionPrincipal{UserID: liveUserID}, nil
}

// verifySessionRole resolves the session's underlying role ARN and compares the
// live RoleId against the stored one. ResolveRoleByARN also compares the stored
// ARN back, so an invented path cannot resolve to a role its ARN does not name.
func (s *STSServiceImpl) verifySessionRole(cred *SessionCredential) (*SessionPrincipal, error) {
	if cred.RoleID == "" {
		return nil, ErrSessionPrincipalLegacy
	}

	roleAccountID, role, err := ResolveRoleByARN(s.iamSvc, cred.UnderlyingRoleARN)
	if err != nil {
		if errors.Is(err, ErrRoleUnresolved) {
			return nil, ErrSessionPrincipalGone
		}
		return nil, fmt.Errorf("resolve session role: %w", err)
	}
	if role == nil {
		return nil, ErrSessionPrincipalGone
	}

	// The role's account was the session's account at mint, so a disagreement
	// means the record no longer describes the principal it was minted for.
	if roleAccountID != cred.AccountID {
		return nil, ErrSessionPrincipalReplaced
	}
	if aws.StringValue(role.RoleId) != cred.RoleID {
		return nil, ErrSessionPrincipalReplaced
	}

	return &SessionPrincipal{
		RoleID:   aws.StringValue(role.RoleId),
		RoleName: aws.StringValue(role.RoleName),
		RoleARN:  aws.StringValue(role.Arn),
	}, nil
}
