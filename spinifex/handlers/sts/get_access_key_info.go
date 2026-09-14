package handlers_sts

import (
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	// Bounds from the STS 2011-06-15 model's AccessKeyIdType. A value outside
	// them cannot name a stored key, so it is refused before the KV read.
	minAccessKeyIDLength = 16
	maxAccessKeyIDLength = 128
)

// GetAccessKeyInfo resolves an access key ID to the account that owns it.
// AWS requires no permission for this call and does not scope it to the caller,
// so no caller identity is threaded in: a key owned by another account resolves
// normally. Long-lived (AKIA) keys resolve from the IAM access-key bucket and
// session (ASIA) keys from the session-credential bucket.
func (s *STSServiceImpl) GetAccessKeyInfo(input *sts.GetAccessKeyInfoInput) (*sts.GetAccessKeyInfoOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	accessKeyID := aws.StringValue(input.AccessKeyId)
	if accessKeyID == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if !isAccessKeyIDShaped(accessKeyID) {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	accountID, err := s.resolveAccessKeyAccount(accessKeyID)
	if err != nil {
		return nil, err
	}
	if accountID == "" {
		// AWS reports an unresolvable key ID as an invalid token. A success with a
		// blank Account would read to a caller as a mismatch rather than a failure.
		slog.Debug("GetAccessKeyInfo: access key ID resolved to no account", "akid", accessKeyID)
		return nil, errors.New(awserrors.ErrorInvalidClientTokenId)
	}

	return &sts.GetAccessKeyInfoOutput{Account: aws.String(accountID)}, nil
}

// resolveAccessKeyAccount returns the owning account of a long-lived or session
// access key ID, or empty when neither store holds it. A dependency fault is
// returned as an error so an outage cannot masquerade as a nonexistent key.
func (s *STSServiceImpl) resolveAccessKeyAccount(accessKeyID string) (string, error) {
	ak, err := s.iamSvc.LookupAccessKey(accessKeyID)
	switch {
	case err == nil:
		if ak == nil || ak.AccountID == "" {
			slog.Error("GetAccessKeyInfo: access key record carries no account", "akid", accessKeyID)
			return "", errors.New(awserrors.ErrorInternalError)
		}
		return ak.AccountID, nil
	case isNoSuchEntity(err):
		// Not a long-lived key; a session key lives in the other bucket.
	default:
		slog.Error("GetAccessKeyInfo: access key lookup failed", "akid", accessKeyID, "err", err)
		return "", errors.New(awserrors.ErrorInternalError)
	}

	cred, err := s.LookupSessionCredential(accessKeyID)
	if err != nil {
		slog.Error("GetAccessKeyInfo: session credential lookup failed", "akid", accessKeyID, "err", err)
		return "", errors.New(awserrors.ErrorInternalError)
	}
	if cred == nil {
		return "", nil
	}
	if cred.AccountID == "" {
		slog.Error("GetAccessKeyInfo: session credential carries no account", "akid", accessKeyID)
		return "", errors.New(awserrors.ErrorInternalError)
	}
	return cred.AccountID, nil
}

// isNoSuchEntity reports whether err is IAM's not-found, as distinct from a
// dependency fault that happens to surface while reading the same bucket.
func isNoSuchEntity(err error) bool {
	code, ok := awserrors.ResolveErrorCode(err)
	return ok && code == awserrors.ErrorIAMNoSuchEntity
}

// isAccessKeyIDShaped reports whether s satisfies the model's AccessKeyIdType:
// 16-128 characters, each an ASCII letter or digit.
func isAccessKeyIDShaped(s string) bool {
	if len(s) < minAccessKeyIDLength || len(s) > maxAccessKeyIDLength {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
