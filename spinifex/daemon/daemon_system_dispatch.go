package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// systemInstanceLaunchEnvelope is the wire format for
// system.LaunchInstance.{type}[.{nodeID}] replies. Either Output or Error
// is set. Encoded as JSON so non-Go consumers stay possible later.
type systemInstanceLaunchEnvelope struct {
	Output *handlers_elbv2.SystemInstanceOutput `json:"output,omitempty"`
	Error  string                               `json:"error,omitempty"`
}

// systemInstanceTerminateEnvelope is the wire format for
// system.TerminateInstance.{instanceID} replies. Empty Error means success.
type systemInstanceTerminateEnvelope struct {
	Error string `json:"error,omitempty"`
}

// handleSystemLaunchInstance is the NATS subscriber for
// system.LaunchInstance.{type}[.{nodeID}] requests. The owning daemon (the
// one whose queue-group subscription wins, or the one whose nodeID-targeted
// subscription is hit) runs LaunchSystemInstance locally and the resulting
// VM stays bound to this node — see handleSystemTerminateInstance for the
// matching teardown path.
func (d *Daemon) handleSystemLaunchInstance(msg *nats.Msg) {
	input := new(handlers_elbv2.SystemInstanceInput)
	if err := json.Unmarshal(msg.Data, input); err != nil {
		slog.Error("system.LaunchInstance: invalid JSON payload", "subject", msg.Subject, "err", err)
		respondWithSystemLaunchError(msg, awserrors.ErrorServerInternal)
		return
	}

	output, err := d.LaunchSystemInstance(input)
	if err != nil {
		slog.Error("system.LaunchInstance: LaunchSystemInstance failed",
			"instanceType", input.InstanceType, "subject", msg.Subject, "err", err)
		respondWithSystemLaunchError(msg, err.Error())
		return
	}

	// Bind a per-instance terminate subscription so future
	// system.TerminateInstance.{id} requests reach the daemon that owns the
	// VM. cleanupSystemTerminateSubscription unwires on shutdown.
	if subErr := d.subscribeSystemTerminate(output.InstanceID); subErr != nil {
		slog.Error("system.LaunchInstance: failed to subscribe terminate subject",
			"instanceId", output.InstanceID, "err", subErr)
	}

	respondWithSystemLaunchOutput(msg, output)
}

// subscribeSystemTerminate registers an owning-node subscription for
// system.TerminateInstance.{instanceID}. Idempotent — calling for an
// already-bound subscription is a no-op.
func (d *Daemon) subscribeSystemTerminate(instanceID string) error {
	if d.natsConn == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	subject := fmt.Sprintf("system.TerminateInstance.%s", instanceID)
	if _, exists := d.natsSubscriptions[subject]; exists {
		return nil
	}
	sub, err := d.natsConn.Subscribe(subject, d.handleSystemTerminateInstance)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", subject, err)
	}
	d.natsSubscriptions[subject] = sub
	return nil
}

// handleSystemTerminateInstance is the NATS subscriber for
// system.TerminateInstance.{instanceID}. Only the daemon that owns the VM
// has a subscription on this subject — other daemons never see the request.
func (d *Daemon) handleSystemTerminateInstance(msg *nats.Msg) {
	// Subject suffix is the instance ID; payload is unused but reserved for
	// future flags. Tolerate empty payloads.
	parts := splitSubjectTail(msg.Subject, "system.TerminateInstance.")
	if parts == "" {
		slog.Error("system.TerminateInstance: subject missing instance ID", "subject", msg.Subject)
		respondWithSystemTerminateError(msg, awserrors.ErrorInvalidInstanceIDNotFound)
		return
	}

	if err := d.TerminateSystemInstance(parts); err != nil {
		slog.Error("system.TerminateInstance: failed",
			"instanceId", parts, "err", err)
		respondWithSystemTerminateError(msg, err.Error())
		return
	}

	// Drop the per-instance terminate subscription now the VM is gone.
	d.mu.Lock()
	subject := fmt.Sprintf("system.TerminateInstance.%s", parts)
	if sub, exists := d.natsSubscriptions[subject]; exists {
		if unsubErr := sub.Unsubscribe(); unsubErr != nil {
			slog.Warn("system.TerminateInstance: failed to unsubscribe", "subject", subject, "err", unsubErr)
		}
		delete(d.natsSubscriptions, subject)
	}
	d.mu.Unlock()

	respondWithSystemTerminateOK(msg)
}

func respondWithSystemLaunchOutput(msg *nats.Msg, out *handlers_elbv2.SystemInstanceOutput) {
	payload, err := json.Marshal(systemInstanceLaunchEnvelope{Output: out})
	if err != nil {
		slog.Error("system.LaunchInstance: marshal output failed", "err", err)
		respondWithSystemLaunchError(msg, awserrors.ErrorServerInternal)
		return
	}
	if err := msg.Respond(payload); err != nil {
		slog.Error("system.LaunchInstance: respond failed", "err", err)
	}
}

func respondWithSystemLaunchError(msg *nats.Msg, errMsg string) {
	if errMsg == "" {
		errMsg = awserrors.ErrorServerInternal
	}
	payload, err := json.Marshal(systemInstanceLaunchEnvelope{Error: errMsg})
	if err != nil {
		// Fall back to bare error payload so the requester at least sees a non-empty reply.
		if respErr := msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorServerInternal)); respErr != nil {
			slog.Error("system.LaunchInstance: respond (error fallback) failed", "err", respErr)
		}
		return
	}
	if err := msg.Respond(payload); err != nil {
		slog.Error("system.LaunchInstance: respond (error) failed", "err", err)
	}
}

func respondWithSystemTerminateOK(msg *nats.Msg) {
	payload, err := json.Marshal(systemInstanceTerminateEnvelope{})
	if err != nil {
		slog.Error("system.TerminateInstance: marshal ok failed", "err", err)
		return
	}
	if err := msg.Respond(payload); err != nil {
		slog.Error("system.TerminateInstance: respond failed", "err", err)
	}
}

func respondWithSystemTerminateError(msg *nats.Msg, errMsg string) {
	if errMsg == "" {
		errMsg = awserrors.ErrorServerInternal
	}
	payload, err := json.Marshal(systemInstanceTerminateEnvelope{Error: errMsg})
	if err != nil {
		if respErr := msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorServerInternal)); respErr != nil {
			slog.Error("system.TerminateInstance: respond (error fallback) failed", "err", respErr)
		}
		return
	}
	if err := msg.Respond(payload); err != nil {
		slog.Error("system.TerminateInstance: respond (error) failed", "err", err)
	}
}

// splitSubjectTail returns the portion of subject after prefix, or empty
// string if subject does not start with prefix. Helper kept local to avoid
// touching utils for a one-liner.
func splitSubjectTail(subject, prefix string) string {
	if len(subject) <= len(prefix) || subject[:len(prefix)] != prefix {
		return ""
	}
	return subject[len(prefix):]
}
