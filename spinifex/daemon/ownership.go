package daemon

import (
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// checkInstanceOwnership verifies the caller owns the instance.
// Returns true if access is allowed, false if denied (error already sent).
// Instances with an empty AccountID (legacy/migration data) are only
// visible to root (GlobalAccountID).
func checkInstanceOwnership(msg *nats.Msg, instanceID, ownerAccountID string) bool {
	callerAccountID := utils.AccountIDFromMsg(msg)

	// Untenanted instance: only root can access.
	if ownerAccountID == "" {
		if callerAccountID != utils.GlobalAccountID {
			slog.Warn("Untenanted instance access denied (not root)",
				"instanceId", instanceID, "callerAccount", callerAccountID)
			respondWithError(msg, awserrors.ErrorInvalidInstanceIDNotFound)
			return false
		}
		return true
	}

	// Normal ownership check
	if callerAccountID != ownerAccountID {
		slog.Warn("Account does not own instance",
			"instanceId", instanceID, "callerAccount", callerAccountID, "ownerAccount", ownerAccountID)
		respondWithError(msg, awserrors.ErrorInvalidInstanceIDNotFound)
		return false
	}
	return true
}

// isInstanceVisible checks if the caller can see this instance (for Describe operations).
// Instances with an empty AccountID (legacy/migration data) are only
// visible to root (GlobalAccountID).
func isInstanceVisible(callerAccountID, ownerAccountID string) bool {
	if ownerAccountID == "" {
		return callerAccountID == utils.GlobalAccountID
	}
	return callerAccountID == ownerAccountID
}

// volumeVisibleTo reports whether callerAccountID may operate on a volume with
// the given tenantID. Volumes with an empty tenantID (legacy/migration
// data) are root-only — without this, the short-circuit
// `tenantID != "" && tenantID != caller` matches every caller and lets any
// tenant attach a legacy/migration volume by ID alone.
func volumeVisibleTo(tenantID, callerAccountID string) bool {
	if tenantID == "" {
		return callerAccountID == utils.GlobalAccountID
	}
	return callerAccountID == tenantID
}
