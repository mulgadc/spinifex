package handlers_sts

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"unicode/utf8"

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

// accessKeyIDRegex is the model's AccessKeyIdType pattern [\w]*, ASCII-only as on AWS.
var accessKeyIDRegex = regexp.MustCompile(`^\w*$`)

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

	var violations []constraintViolation
	if !accessKeyIDRegex.MatchString(accessKeyID) {
		violations = append(violations, constraintViolation{"accessKeyId", accessKeyID,
			`Member must satisfy regular expression pattern: [\w]*`})
	}
	if n := utf8.RuneCountInString(accessKeyID); n < minAccessKeyIDLength {
		violations = append(violations, constraintViolation{"accessKeyId", accessKeyID,
			fmt.Sprintf("Member must have length greater than or equal to %d", minAccessKeyIDLength)})
	} else if n > maxAccessKeyIDLength {
		violations = append(violations, constraintViolation{"accessKeyId", accessKeyID,
			fmt.Sprintf("Member must have length less than or equal to %d", maxAccessKeyIDLength)})
	}
	if err := validationError(violations); err != nil {
		return nil, err
	}

	accountID, err := s.resolveAccessKeyAccount(accessKeyID)
	if err != nil {
		return nil, err
	}
	if accountID == "" {
		// AWS reports an unresolvable key ID as a validation error. A success with a
		// blank Account would read to a caller as a mismatch rather than a failure.
		slog.Debug("GetAccessKeyInfo: access key ID resolved to no account", "akid", accessKeyID)
		return nil, awserrors.Errorf(awserrors.ErrorValidationError, "Access key ID is not valid.")
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
