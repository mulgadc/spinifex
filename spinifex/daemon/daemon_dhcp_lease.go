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
