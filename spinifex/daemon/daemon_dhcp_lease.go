package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/nats-io/nats.go"
)

// leaseRebindTimeout bounds a single reconcile. It exceeds the vpc.add-nat
// request-reply budget so a slow NAT commit reports its own error rather than
// wearing a deadline from here.
const leaseRebindTimeout = 60 * time.Second

// eipPublicIPRebinder moves an allocation's public address. Asserted rather than
// required so the disabled EIP stub (no external IPAM) is simply skipped.
type eipPublicIPRebinder interface {
	RebindPublicIP(ctx context.Context, allocationID, oldIP, newIP string) error
}

// handleDHCPLeaseChanged reconciles the record naming an address whose lease was
// re-issued elsewhere. vpcd has already released the old address, so a failure
// here means an API-visible record still advertises an address nothing holds —
// hence the error goes back on the reply rather than only to the log.
func (d *Daemon) handleDHCPLeaseChanged(msg *nats.Msg) {
	var req dhcp.LeaseChangedRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		respondLeaseChanged(msg, fmt.Errorf("decode lease-changed request: %w", err))
		return
	}
	if req.ClientID == "" || req.NewIP == "" {
		respondLeaseChanged(msg, fmt.Errorf("lease-changed request needs client_id and new_ip"))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), leaseRebindTimeout)
	defer cancel()
	respondLeaseChanged(msg, d.rebindLeaseRecord(ctx, req))
}

// rebindLeaseRecord dispatches to the service owning the moved address.
func (d *Daemon) rebindLeaseRecord(ctx context.Context, req dhcp.LeaseChangedRequest) error {
	switch req.Purpose {
	case dhcp.PurposeEIP:
		rebinder, ok := d.eipService.(eipPublicIPRebinder)
		if !ok {
			return fmt.Errorf("EIP service cannot rebind %s: external IPAM disabled", req.ClientID)
		}
		return rebinder.RebindPublicIP(ctx, req.ClientID, req.OldIP, req.NewIP)

	case dhcp.PurposeENIPublic:
		// The client-id is the ENI id when one is known, and the instance id
		// otherwise; only the former identifies a record to move.
		if d.vpcService == nil {
			return fmt.Errorf("no VPC service to rebind ENI public IP for %s", req.ClientID)
		}
		return d.vpcService.RebindENIPublicIP(ctx, req.ClientID, req.OldIP, req.NewIP)

	default:
		return fmt.Errorf("no owner for %q lease %s: address moved %s -> %s",
			req.Purpose, req.ClientID, req.OldIP, req.NewIP)
	}
}

// ownerCheckTimeout bounds one existence lookup. Short: the reaper runs on a
// timer and an unanswered check keeps the lease, so waiting longer buys nothing.
const ownerCheckTimeout = 15 * time.Second

// eipAllocationChecker reports whether an allocation record survives. Asserted
// rather than required so the disabled EIP stub is answered as unknown.
type eipAllocationChecker interface {
	AllocationExists(ctx context.Context, allocationID string) (bool, error)
}

// handleDHCPOwnerCheck answers whether the resource behind a lease still exists.
// The reaper releases addresses on a "gone", so anything this cannot establish
// with certainty is reported unknown.
func (d *Daemon) handleDHCPOwnerCheck(msg *nats.Msg) {
	var req dhcp.OwnerCheckRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		respondOwnerCheck(msg, dhcp.OwnerStatusUnknown, fmt.Errorf("decode owner-check request: %w", err))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), ownerCheckTimeout)
	defer cancel()

	status, err := d.leaseOwnerStatus(ctx, req)
	respondOwnerCheck(msg, status, err)
}

// leaseOwnerStatus resolves a lease to its owning record, per purpose.
func (d *Daemon) leaseOwnerStatus(ctx context.Context, req dhcp.OwnerCheckRequest) (string, error) {
	switch req.Purpose {
	case dhcp.PurposeEIP:
		checker, ok := d.eipService.(eipAllocationChecker)
		if !ok {
			return dhcp.OwnerStatusUnknown, fmt.Errorf("EIP service cannot resolve %s: external IPAM disabled", req.ClientID)
		}
		return existsToStatus(checker.AllocationExists(ctx, req.ClientID))

	case dhcp.PurposeENIPublic:
		if d.vpcService == nil {
			return dhcp.OwnerStatusUnknown, fmt.Errorf("no VPC service to resolve ENI %s", req.ClientID)
		}
		return existsToStatus(d.vpcService.ENIExists(ctx, req.ClientID))

	case dhcp.PurposeGatewayLRP:
		if d.vpcService == nil {
			return dhcp.OwnerStatusUnknown, fmt.Errorf("no VPC service to resolve VPC %s", req.VPCID)
		}
		if req.VPCID == "" {
			return dhcp.OwnerStatusUnknown, fmt.Errorf("gw-lrp lease %s carries no vpc_id", req.ClientID)
		}
		return existsToStatus(d.vpcService.VPCExists(ctx, req.VPCID))

	default:
		// A purpose this daemon does not know may well have a live owner it
		// cannot see, so it is never reported gone.
		return dhcp.OwnerStatusUnknown, fmt.Errorf("no owner lookup for %q lease %s", req.Purpose, req.ClientID)
	}
}

func existsToStatus(exists bool, err error) (string, error) {
	if err != nil {
		return dhcp.OwnerStatusUnknown, err
	}
	if exists {
		return dhcp.OwnerStatusAlive, nil
	}
	return dhcp.OwnerStatusGone, nil
}

// respondOwnerCheck replies with the verdict, carrying err's text when the
// lookup failed. The error is informational — the status already says unknown.
func respondOwnerCheck(msg *nats.Msg, status string, err error) {
	reply := dhcp.OwnerCheckReply{Status: status}
	if err != nil {
		reply.Error = err.Error()
		slog.Warn("daemon: dhcp lease owner check failed; reporting unknown", "err", err)
	}
	data, marshalErr := json.Marshal(reply)
	if marshalErr != nil {
		slog.Error("daemon: marshal owner-check reply failed", "err", marshalErr)
		return
	}
	if msg.Reply == "" {
		slog.Error("daemon: owner-check request carried no reply subject", "outcome", string(data))
		return
	}
	if respErr := msg.Respond(data); respErr != nil {
		slog.Error("daemon: respond to owner-check failed", "err", respErr)
	}
}

// respondLeaseChanged replies with err's text, or empty on success. A missing
// reply subject means the caller published rather than requested, which would
// silently drop the outcome, so it is logged.
func respondLeaseChanged(msg *nats.Msg, err error) {
	reply := dhcp.LeaseChangedReply{}
	if err != nil {
		reply.Error = err.Error()
		slog.Error("daemon: dhcp lease rebind failed; a resource record still names a released address", "err", err)
	}
	data, marshalErr := json.Marshal(reply)
	if marshalErr != nil {
		slog.Error("daemon: marshal lease-changed reply failed", "err", marshalErr)
		return
	}
	if msg.Reply == "" {
		slog.Error("daemon: lease-changed request carried no reply subject", "outcome", string(data))
		return
	}
	if respErr := msg.Respond(data); respErr != nil {
		slog.Error("daemon: respond to lease-changed failed", "err", respErr)
	}
}
