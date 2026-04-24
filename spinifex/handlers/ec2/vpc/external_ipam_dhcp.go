package handlers_ec2_vpc

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/dhcp"
	"github.com/nats-io/nats.go"
)

// dhcpNATSTimeout bounds the request/reply to spinifex-vpcd. Picked to
// comfortably exceed the DORA handshake (15 s default + server jitter +
// ~10 s of slack for NAK + fresh DISCOVER fallbacks). A var rather than
// a const so tests can drive the timeout error path without waiting
// 20 s per table entry.
var dhcpNATSTimeout = 20 * time.Second

// dhcpLeaseResult is the data ExternalIPAM needs from a successful acquire.
// It maps directly from dhcp.AcquireReplyMsg; kept as a handler-side type
// so the IPAM ledger records the fields without coupling to the NATS wire
// struct.
type dhcpLeaseResult struct {
	IP          string
	SubnetMask  string
	Routers     []string
	DNS         []string
	ServerID    string
	HWAddr      string
	ExpiresUnix int64
}

// ObtainDHCPLease asks spinifex-vpcd to acquire a DHCP lease on the given
// bridge, identifying us with option 61 (client-id), option 12 (hostname)
// and option 60 (vendor class). Blocks until vpcd replies or
// dhcpNATSTimeout expires. The Manager-side handler is idempotent: a
// second call with the same clientID while a live lease exists returns
// the same lease without a fresh DORA, so CAS retry loops on the caller
// are safe.
func ObtainDHCPLease(nc *nats.Conn, bridge, clientID, hostname, vendorClass, poolName string) (dhcpLeaseResult, error) {
	if nc == nil {
		return dhcpLeaseResult{}, fmt.Errorf("DHCP lease: NATS connection is required")
	}
	if bridge == "" {
		return dhcpLeaseResult{}, fmt.Errorf("DHCP lease: bridge name is required")
	}
	if clientID == "" {
		return dhcpLeaseResult{}, fmt.Errorf("DHCP lease: client ID is required")
	}

	req := dhcp.AcquireRequestMsg{
		Bridge:      bridge,
		ClientID:    clientID,
		Hostname:    hostname,
		VendorClass: vendorClass,
		PoolName:    poolName,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return dhcpLeaseResult{}, fmt.Errorf("marshal dhcp acquire request: %w", err)
	}

	msg, err := nc.Request(dhcp.TopicAcquire, data, dhcpNATSTimeout)
	if err != nil {
		return dhcpLeaseResult{}, fmt.Errorf("dhcp acquire NATS request (client %s): %w", clientID, err)
	}

	var reply dhcp.AcquireReplyMsg
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return dhcpLeaseResult{}, fmt.Errorf("unmarshal dhcp acquire reply: %w", err)
	}
	if reply.Error != "" {
		return dhcpLeaseResult{}, fmt.Errorf("dhcp acquire (client %s): %s", clientID, reply.Error)
	}

	slog.Info("DHCP lease obtained",
		"bridge", bridge,
		"client_id", clientID,
		"ip", reply.IP,
		"server_id", reply.ServerID,
		"expires_unix", reply.ExpiresUnix,
	)

	return dhcpLeaseResult{
		IP:          reply.IP,
		SubnetMask:  reply.SubnetMask,
		Routers:     reply.Routers,
		DNS:         reply.DNS,
		ServerID:    reply.ServerID,
		HWAddr:      reply.HWAddr,
		ExpiresUnix: reply.ExpiresUnix,
	}, nil
}

// ReleaseDHCPLease asks spinifex-vpcd to release the lease identified by
// clientID. Returns nil (silently) when nc or clientID is empty so that
// callers in clean-up paths can invoke it unconditionally.
func ReleaseDHCPLease(nc *nats.Conn, clientID string) error {
	if nc == nil || clientID == "" {
		return nil
	}

	data, err := json.Marshal(dhcp.ReleaseRequestMsg{ClientID: clientID})
	if err != nil {
		return fmt.Errorf("marshal dhcp release request: %w", err)
	}

	msg, err := nc.Request(dhcp.TopicRelease, data, dhcpNATSTimeout)
	if err != nil {
		return fmt.Errorf("dhcp release NATS request (client %s): %w", clientID, err)
	}

	var reply dhcp.ReleaseReplyMsg
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return fmt.Errorf("unmarshal dhcp release reply: %w", err)
	}
	if reply.Error != "" {
		return fmt.Errorf("dhcp release (client %s): %s", clientID, reply.Error)
	}

	slog.Info("DHCP lease released", "client_id", clientID)
	return nil
}
