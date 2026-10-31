package gateway

import (
	"errors"
	"log/slog"

	spxarn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

// buildCallerARN composes the caller ARN: assumed-role uses the sealed assumedRoleARN,
// root uses arn:aws:iam::{aid}:root, user uses its stored userARN, path included,
// or arn:aws:iam::{aid}:user/{identity} when there is no record to read it from.
func buildCallerARN(accountID, identity, principalType, assumedRoleARN, userARN string) (string, error) {
	switch principalType {
	case principalTypeAssumedRole:
		if assumedRoleARN == "" {
			slog.Error("buildCallerARN: assumed-role principal without ARN")
			return "", errors.New(awserrors.ErrorInternalError)
		}
		return assumedRoleARN, nil
	case principalTypeRoot:
		return spxarn.FormatIAMRoot(accountID), nil
	case principalTypeUser:
		if identity == "root" && accountID == awsidentifiers.GlobalAccountID {
			return spxarn.FormatIAMRoot(accountID), nil
		}
		if identity == "" {
			slog.Error("buildCallerARN: user principal without identity")
			return "", errors.New(awserrors.ErrorInternalError)
		}
		if userARN != "" {
			return userARN, nil
		}
		return spxarn.FormatIAMPath(spxarn.IAMUser, accountID, "/", identity), nil
	default:
		slog.Error("buildCallerARN: unknown principal type", "principalType", principalType)
		return "", errors.New(awserrors.ErrorInternalError)
	}
}
