package handlers_sts

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	// GetSessionToken duration bounds differ from AssumeRole: AWS permits a
	// session up to 36h (vs. the 12h role-session ceiling) and defaults to 12h.
	// The floor is the STS-wide minimum (minDurationSeconds, 900s).
	getSessionTokenMaxDuration     int64 = 129600 // 36h
	getSessionTokenDefaultDuration int64 = 43200  // 12h
)

// GetSessionToken exchanges the calling IAM user's long-lived credentials for
// short-lived ASIA session credentials bound to the SAME user identity. Unlike
// AssumeRole, the resulting session resolves back to arn:aws:iam::A:user/N and
// IAM policy is evaluated against that user — see the user branch of the SigV4
// ASIA resolve path. The caller identity is resolved by the gateway and passed
// in as plain strings to keep the handler unit-testable.
//
// AWS forbids calling GetSessionToken with temporary credentials, so only a
// long-lived user principal (AKIA → principalType "user") is accepted. Assumed
// -role callers are rejected by principal type; session callers — including a
// GetSessionToken session, which resolves back to principalType "user" — are
// rejected by their ASIA access-key prefix. Root is a long-lived user and, per
// AWS, may call GetSessionToken.
func (s *STSServiceImpl) GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
	if input == nil {
		// `aws sts get-session-token` with no arguments sends an empty body;
		// an absent input is the common case, not an error.
		input = &sts.GetSessionTokenInput{}
	}

	if callerPrincipalType != principalTypeUser {
		slog.Warn("GetSessionToken denied: caller is not a long-lived user principal",
			"account_id", callerAccountID, "principal_type", callerPrincipalType)
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	if strings.HasPrefix(callerAccessKeyID, SessionAccessKeyIDPrefix) {
		// A GetSessionToken session resolves back to principalType "user", so the
		// check above cannot catch it — the ASIA prefix is the only signal that
		// the caller holds temporary credentials. Rejecting it stops a session
		// from minting a fresh session and rolling its lifetime forward forever.
		slog.Warn("GetSessionToken denied: caller is using temporary (session) credentials",
			"account_id", callerAccountID, "akid", callerAccessKeyID)
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	if callerAccountID == "" || callerUserName == "" {
		// A user principal with no account or name is a mis-wired caller, not a
		// client error — fail loud rather than mint a session with an empty name.
		slog.Error("GetSessionToken: user principal missing account or name",
			"account_id", callerAccountID, "user_name", callerUserName)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	// MFA (SerialNumber/TokenCode) is out of scope: reject rather than silently
	// ignore so a caller never believes an MFA condition was enforced.
	if aws.StringValue(input.SerialNumber) != "" || aws.StringValue(input.TokenCode) != "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	duration := getSessionTokenDefaultDuration
	if input.DurationSeconds != nil {
		duration = clampGetSessionTokenDuration(*input.DurationSeconds)
	}

	cred, plainSecret, plainToken, err := s.mintSession(userEnvelope(callerAccountID, callerUserName), duration)
	if err != nil {
		return nil, err
	}

	slog.Info("GetSessionToken success",
		"account_id", callerAccountID,
		"user_name", callerUserName,
		"akid", cred.AccessKeyID,
		"expires_at", cred.ExpiresAt,
	)

	return &sts.GetSessionTokenOutput{
		Credentials: &sts.Credentials{
			AccessKeyId:     aws.String(cred.AccessKeyID),
			SecretAccessKey: aws.String(plainSecret),
			SessionToken:    aws.String(plainToken),
			Expiration:      aws.Time(cred.ExpiresAt),
		},
	}, nil
}

// clampGetSessionTokenDuration coerces a requested duration into the permitted
// [900, 129600] window. A nil DurationSeconds is handled by the caller (default
// 12h); this only sees an explicit value, which AWS clamps rather than rejects.
func clampGetSessionTokenDuration(requested int64) int64 {
	if requested < minDurationSeconds {
		return minDurationSeconds
	}
	if requested > getSessionTokenMaxDuration {
		return getSessionTokenMaxDuration
	}
	return requested
}

// userEnvelope derives the session envelope for a GetSessionToken user session:
// PrincipalType "user", SessionName = the IAM user name, and no assumed-role
// fields. resolveSessionAKID reads PrincipalType back to rebuild the caller as
// arn:aws:iam::A:user/N rather than synthesising an assumed-role ARN.
func userEnvelope(accountID, userName string) sessionEnvelope {
	return sessionEnvelope{
		PrincipalType: principalTypeUser,
		AccountID:     accountID,
		SessionName:   userName,
	}
}
