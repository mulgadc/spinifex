package daemon

import (
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// checkInstanceOwnership verifies the caller owns the instance. Returns true if
// allowed; false after sending an error. Empty ownerAccountID is root-only.
func checkInstanceOwnership(nodeID string, msg *nats.Msg, instanceID, ownerAccountID string) bool {
	callerAccountID := utils.AccountIDFromMsg(msg)

	if ownerAccountID == "" {
		if callerAccountID != utils.GlobalAccountID {
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
