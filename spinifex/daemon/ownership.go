package daemon

import (
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// checkInstanceOwnership verifies the caller owns the instance. Returns true if
// allowed; false after sending an error. Empty ownerAccountID is root-only.
func checkInstanceOwnership(nodeID string, msg *nats.Msg, instanceID, ownerAccountID string) bool {
	callerAccountID := natsmsg.AccountIDFromMsg(msg)

	if ownerAccountID == "" {
		if callerAccountID != awsidentifiers.GlobalAccountID {
			slog.Warn("Untenanted instance access denied (not root)",
				"instanceId", instanceID, "callerAccount", callerAccountID)
			respondWithError(nodeID, msg, awserrors.ErrorInvalidInstanceIDNotFound)
			return false
		}
		return true
	}

	if callerAccountID != ownerAccountID {
		slog.Warn("Account does not own instance",
			"instanceId", instanceID, "callerAccount", callerAccountID, "ownerAccount", ownerAccountID)
		respondWithError(nodeID, msg, awserrors.ErrorInvalidInstanceIDNotFound)
		return false
	}
	return true
}
