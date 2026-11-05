package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/nats-io/nats.go"
)

// nodeSystemLaunchTimeout caps a node-targeted system.LaunchInstance round
// trip. Sized for a full-AMI control-plane VM boot (clone root + cloud-init
// seed + QEMU start) on a remote host, well above the local microVM path.
const nodeSystemLaunchTimeout = 5 * time.Minute

// systemInstanceLaunchEnvelope is the wire format for
// system.LaunchInstance.{type}[.{nodeID}] replies. Either Output or Error
// is set. Encoded as JSON so non-Go consumers stay possible later.
type systemInstanceLaunchEnvelope struct {
	Output *elbv2domain.SystemInstanceOutput `json:"output,omitempty"`
	Error  string                            `json:"error,omitempty"`
}

// systemInstanceTerminateEnvelope is the wire format for
// system.TerminateInstance.{instanceID} replies. Empty Error means success.
type systemInstanceTerminateEnvelope struct {
	Error string `json:"error,omitempty"`
}

// systemLaunchAction names the metric for every system.LaunchInstance subject.
// The subject carries the instance type and node, which the action must not.
const systemLaunchAction = "system.LaunchInstance"

// handleSystemLaunchInstance is the NATS subscriber for system.LaunchInstance.
// The launch runs in its own goroutine so a multi-minute VM boot cannot
// head-of-line block concurrent launches to the same node.
func (d *Daemon) handleSystemLaunchInstance(msg *nats.Msg) string {
	return d.dispatchSystemLaunch(msg, systemLaunchAction, d.LaunchSystemInstance)
}

// systemLaunchFunc launches one system instance from a decoded request.
type systemLaunchFunc func(*elbv2domain.SystemInstanceInput) (*elbv2domain.SystemInstanceOutput, error)

// dispatchSystemLaunch runs launch on its own goroutine and records the
// request under action.
func (d *Daemon) dispatchSystemLaunch(msg *nats.Msg, action string, launch systemLaunchFunc) string {
	start := time.Now()
	d.systemDispatchWg.Go(func() {
		outcome := outcomeError
		defer func() {
			if r := recover(); r != nil {
				slog.Error("system.LaunchInstance: handler panic", "subject", msg.Subject, "panic", r)
				respondWithSystemLaunchError(msg, awserrors.ErrorServerInternal)
			}
			otelsetup.RecordRequest(context.Background(), action, outcome, time.Since(start))
		}()
		outcome = d.serveSystemLaunch(msg, launch)
	})
	return outcomeDeferred
}

// serveSystemLaunchInstance is the synchronous body of handleSystemLaunchInstance,
// run on a per-request goroutine.
func (d *Daemon) serveSystemLaunchInstance(msg *nats.Msg) string {
	return d.serveSystemLaunch(msg, d.LaunchSystemInstance)
}

func (d *Daemon) serveSystemLaunch(msg *nats.Msg, launch systemLaunchFunc) string {
	input := new(elbv2domain.SystemInstanceInput)
	if err := json.Unmarshal(msg.Data, input); err != nil {
		slog.Error("system.LaunchInstance: invalid JSON payload", "subject", msg.Subject, "err", err)
		respondWithSystemLaunchError(msg, awserrors.ErrorServerInternal)
		return outcomeError
	}

	output, err := launch(input)
	if err != nil {
		slog.Error("system launch failed",
			"instanceType", input.InstanceType, "subject", msg.Subject, "err", err)
		respondWithSystemLaunchError(msg, err.Error())
		return outcomeError
	}

	// Bind a per-instance terminate subscription so future
	// system.TerminateInstance.{id} requests reach the daemon that owns the
	// VM. cleanupSystemTerminateSubscription unwires on shutdown.
	if subErr := d.subscribeSystemTerminate(output.InstanceID); subErr != nil {
		slog.Error("system.LaunchInstance: failed to subscribe terminate subject",
			"instanceId", output.InstanceID, "err", subErr)
	}

	d.mu.Lock()
	rebootErr := d.subscribeSystemRebootLocked(output.InstanceID)
	d.mu.Unlock()
	if rebootErr != nil {
		slog.Error("system.LaunchInstance: failed to subscribe reboot subject",
			"instanceId", output.InstanceID, "err", rebootErr)
	}

	respondWithSystemLaunchOutput(msg, output)
	return outcomeSuccess
}

// LaunchSystemInstanceOnNode launches a system VM on a specific host.
// An empty nodeID or the local node runs in-process; any other node is
// reached via system.LaunchInstance.{type}.{nodeID} — the VM stays on that node.
func (d *Daemon) LaunchSystemInstanceOnNode(nodeID string, input *elbv2domain.SystemInstanceInput) (*elbv2domain.SystemInstanceOutput, error) {
	if nodeID == "" || nodeID == d.node {
		return d.LaunchSystemInstance(input)
	}
	if d.natsConn == nil {
		return nil, fmt.Errorf("system instance: cannot target node %s without a NATS connection", nodeID)
	}
	if input == nil || input.InstanceType == "" {
		return nil, fmt.Errorf("system instance input missing InstanceType")
	}
	// An older receiver would ignore the kind and boot a load balancer.
	if err := input.RefuseNonLoadBalancerKind(); err != nil {
		return nil, err
	}

	return d.requestSystemLaunch(fmt.Sprintf("system.LaunchInstance.%s.%s", input.InstanceType, nodeID), input)
}

// requestSystemLaunch sends one launch to a node-targeted subject and decodes
// its envelope. A no-responder error stays wrapped for the caller to classify.
func (d *Daemon) requestSystemLaunch(subject string, input *elbv2domain.SystemInstanceInput) (*elbv2domain.SystemInstanceOutput, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal SystemInstanceInput: %w", err)
	}

	reply, err := d.natsConn.Request(subject, payload, nodeSystemLaunchTimeout)
	if err != nil {
		return nil, fmt.Errorf("nats request %s: %w", subject, err)
	}

	var env systemInstanceLaunchEnvelope
	if err := json.Unmarshal(reply.Data, &env); err != nil {
		return nil, fmt.Errorf("decode launch reply: %w", err)
	}
	if env.Error != "" {
		return nil, fmt.Errorf("%s", env.Error)
	}
	if env.Output == nil {
		return nil, fmt.Errorf("launch reply missing output payload")
	}
	return env.Output, nil
}

// systemTerminateRemoteTimeout caps a routed system.TerminateInstance round
// trip. Sized for qemu stop + volume teardown + ENI cascade on a remote host.
const systemTerminateRemoteTimeout = 90 * time.Second

// terminateSystemInstanceRemote routes a terminate to the owning daemon over
// system.TerminateInstance.{id} (only the owner subscribes). A no-responder
// reply means no node owns the VM, so it is genuinely gone — reported as
// ErrSystemInstanceNotFound, which callers treat as idempotent success.
func (d *Daemon) terminateSystemInstanceRemote(instanceID string) error {
	if d.natsConn == nil {
		return fmt.Errorf("%w: %s", systeminstance.ErrSystemInstanceNotFound, instanceID)
	}
	subject := fmt.Sprintf("system.TerminateInstance.%s", instanceID)
	reply, err := d.natsConn.Request(subject, nil, systemTerminateRemoteTimeout)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) {
			slog.Debug("terminateSystemInstanceRemote: no owner subscribed; VM already gone",
				"instanceId", instanceID)
			return fmt.Errorf("%w: %s", systeminstance.ErrSystemInstanceNotFound, instanceID)
		}
		return fmt.Errorf("route terminate %s: %w", instanceID, err)
	}
	var env systemInstanceTerminateEnvelope
	if err := json.Unmarshal(reply.Data, &env); err != nil {
		return fmt.Errorf("decode routed terminate reply %s: %w", instanceID, err)
	}
	if env.Error != "" {
		return routedSystemError("terminate", instanceID, env.Error)
	}
	slog.Info("terminateSystemInstanceRemote: owner terminated VM", "instanceId", instanceID)
	return nil
}

// routedSystemError maps an owner's error text back to the sentinel it carried,
// so callers keep telling gone, customer-owned and unreadable apart.
func routedSystemError(op, instanceID, text string) error {
	for _, sentinel := range []error{
		systeminstance.ErrSystemInstanceNotFound,
		systeminstance.ErrSystemInstanceNotSystem,
		systeminstance.ErrSystemInstanceOwnershipUnknown,
	} {
		if strings.Contains(text, sentinel.Error()) {
			return fmt.Errorf("%w: %s", sentinel, instanceID)
		}
	}
	return fmt.Errorf("routed %s %s: %s", op, instanceID, text)
}

// rebootSystemInstanceRemote routes a reboot to the owning daemon over
// system.RebootInstance.{id}. No responder means the owner is unreachable and
// is reported as ErrSystemInstanceNotFound; the caller retries and must not
// count it as a repair.
func (d *Daemon) rebootSystemInstanceRemote(instanceID string) error {
	if d.natsConn == nil {
		return fmt.Errorf("%w: %s", systeminstance.ErrSystemInstanceNotFound, instanceID)
	}
	subject := systemRebootSubject(instanceID)
	reply, err := d.natsConn.Request(subject, nil, systemRebootRemoteTimeout)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) {
			return fmt.Errorf("%w: %s", systeminstance.ErrSystemInstanceNotFound, instanceID)
		}
		return fmt.Errorf("route reboot %s: %w", instanceID, err)
	}
	var env systemInstanceTerminateEnvelope
	if err := json.Unmarshal(reply.Data, &env); err != nil {
		return fmt.Errorf("decode routed reboot reply %s: %w", instanceID, err)
	}
	if env.Error != "" {
		return routedSystemError("reboot", instanceID, env.Error)
	}
	return nil
}

// systemRebootRemoteTimeout caps a routed reboot round trip. The owner admits
// the reboot and returns; the guest restart runs in the background.
const systemRebootRemoteTimeout = 15 * time.Second

func systemRebootSubject(instanceID string) string {
	return "system.RebootInstance." + instanceID
}

// subscribeSystemRebootLocked registers the owning-node subscription for
// system.RebootInstance.{instanceID}. Idempotent; the caller holds d.mu.
func (d *Daemon) subscribeSystemRebootLocked(instanceID string) error {
	if d.natsConn == nil {
		return nil
	}
	subject := systemRebootSubject(instanceID)
	if _, exists := d.natsSubscriptions[subject]; exists {
		return nil
	}
	sub, err := d.natsConn.Subscribe(subject, d.handleSystemRebootInstance)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", subject, err)
	}
	d.natsSubscriptions[subject] = sub
	return nil
}

// subscribeSystemReboot is subscribeSystemRebootLocked for callers without d.mu.
func (d *Daemon) subscribeSystemReboot(instanceID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.subscribeSystemRebootLocked(instanceID)
}

// handleSystemRebootInstance is the NATS subscriber for
// system.RebootInstance.{instanceID}. Only the owning daemon subscribes.
func (d *Daemon) handleSystemRebootInstance(msg *nats.Msg) {
	d.systemDispatchWg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("system.RebootInstance: handler panic", "subject", msg.Subject, "panic", r)
				respondWithSystemRebootReply(msg, awserrors.ErrorServerInternal)
			}
		}()
		id, ok := strings.CutPrefix(msg.Subject, "system.RebootInstance.")
		if !ok || id == "" {
			respondWithSystemRebootReply(msg, awserrors.ErrorInvalidInstanceIDNotFound)
			return
		}
		// The local body, not the routing wrapper: only the owner subscribes.
		if err := d.rebootSystemInstanceLocal(id); err != nil {
			respondWithSystemRebootReply(msg, err.Error())
			return
		}
		respondWithSystemRebootReply(msg, "")
	})
}

func respondWithSystemRebootReply(msg *nats.Msg, errMsg string) {
	payload, err := json.Marshal(systemInstanceTerminateEnvelope{Error: errMsg})
	if err != nil {
		slog.Error("system.RebootInstance: marshal reply failed", "err", err)
		return
	}
	if err := msg.Respond(payload); err != nil {
		slog.Error("system.RebootInstance: respond failed", "err", err)
	}
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
	return d.subscribeSystemTerminateLocked(instanceID)
}

// subscribeSystemTerminateLocked is the body of subscribeSystemTerminate for
// callers that already hold d.mu (e.g. onInstanceUpHook). Idempotent.
func (d *Daemon) subscribeSystemTerminateLocked(instanceID string) error {
	if d.natsConn == nil {
		return nil
	}
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
// system.TerminateInstance.{instanceID}. Only the owning daemon subscribes.
// Teardown runs in its own goroutine to avoid head-of-line blocking.
func (d *Daemon) handleSystemTerminateInstance(msg *nats.Msg) {
	d.systemDispatchWg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("system.TerminateInstance: handler panic", "subject", msg.Subject, "panic", r)
				respondWithSystemTerminateError(msg, awserrors.ErrorServerInternal)
			}
		}()
		d.serveSystemTerminateInstance(msg)
	})
}

// serveSystemTerminateInstance is the synchronous body of
// handleSystemTerminateInstance, run on a per-request goroutine.
func (d *Daemon) serveSystemTerminateInstance(msg *nats.Msg) {
	// Subject suffix is the instance ID; payload is unused but reserved for
	// future flags. Tolerate empty payloads.
	parts, ok := strings.CutPrefix(msg.Subject, "system.TerminateInstance.")
	if !ok || parts == "" {
		slog.Error("system.TerminateInstance: subject missing instance ID", "subject", msg.Subject)
		respondWithSystemTerminateError(msg, awserrors.ErrorInvalidInstanceIDNotFound)
		return
	}

	// The local body, not the routing wrapper: only the owning node subscribes,
	// so the VM is local here. Routing from the handler could loop back to this
	// same subscription if the VM raced to gone.
	if err := d.terminateSystemInstanceLocal(parts); err != nil {
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
	if sub, exists := d.natsSubscriptions[systemRebootSubject(parts)]; exists {
		_ = sub.Unsubscribe()
		delete(d.natsSubscriptions, systemRebootSubject(parts))
	}
	d.mu.Unlock()

	respondWithSystemTerminateOK(msg)
}

func respondWithSystemLaunchOutput(msg *nats.Msg, out *elbv2domain.SystemInstanceOutput) {
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
		if respErr := msg.Respond(awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal)); respErr != nil {
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
		if respErr := msg.Respond(awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal)); respErr != nil {
			slog.Error("system.TerminateInstance: respond (error fallback) failed", "err", respErr)
		}
		return
	}
	if err := msg.Respond(payload); err != nil {
		slog.Error("system.TerminateInstance: respond (error) failed", "err", err)
	}
}
