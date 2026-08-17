package dhcp

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// NATS subjects for daemon ↔ vpcd DHCP RPCs. Non-AZ-prefixed: the
// bridge-owning vpcd is exactly one process per AZ.
const (
	TopicAcquire = "vpc.dhcp.acquire"
	TopicRelease = "vpc.dhcp.release"
	// TopicDrain asks the bridge-owning vpcd to DHCPRELEASE every lease it
	// currently holds. Invoked on cluster teardown so an env reset returns
	// its leases to the upstream pool instead of stranding them until TTL.
	TopicDrain = "vpc.dhcp.drain"
	// TopicLeaseChanged announces that a lease came back on a different
	// address. Flows the opposite way to the others — vpcd asks the daemon to
	// reconcile, because the records naming these addresses (an EIP's
	// PublicIp, an ENI's PublicIpAddress) live daemon-side.
	TopicLeaseChanged = "vpc.dhcp.lease-changed"
	// TopicOwnerCheck asks whether the resource a lease was taken for still
	// exists. Daemon-ward for the same reason as TopicLeaseChanged: vpcd holds
	// the leases, the daemon holds the records.
	TopicOwnerCheck = "vpc.dhcp.owner-check"
)

// LeaseChangedRequest asks the owner of a lease's resource record to move it
// onto NewIP. Purpose selects the owning record; ClientID is that record's id
// (an allocation id for EIPs, an ENI id for auto-assigned public IPs).
type LeaseChangedRequest struct {
	ClientID string `json:"client_id"`
	Purpose  string `json:"purpose"`
	PoolName string `json:"pool_name,omitempty"`
	VPCID    string `json:"vpc_id,omitempty"`
	OldIP    string `json:"old_ip"`
	NewIP    string `json:"new_ip"`
}

// LeaseChangedReply carries an empty Error on success. A failure means the
// record still names an address vpcd no longer holds, so callers surface it.
type LeaseChangedReply struct {
	Error string `json:"error,omitempty"`
}

// OwnerCheckRequest asks whether the resource behind a lease still exists.
// ClientID is the resource's own id for EIP and ENI-public leases; VPCID
// identifies a gateway LRP lease.
type OwnerCheckRequest struct {
	ClientID string `json:"client_id"`
	Purpose  string `json:"purpose"`
	VPCID    string `json:"vpc_id,omitempty"`
}

// OwnerCheckReply reports "alive", "gone" or "unknown". Anything the responder
// cannot answer stays unknown, which the reaper treats as keep — a lookup
// failure must never be read as a deletion.
type OwnerCheckReply struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Wire values for OwnerCheckReply.Status.
const (
	OwnerStatusAlive   = "alive"
	OwnerStatusGone    = "gone"
	OwnerStatusUnknown = "unknown"
)

// ParseOwnerStatus maps a wire status onto OwnerStatus. Anything unrecognised
// is unknown, so a newer responder's verdict is never mistaken for "gone".
func ParseOwnerStatus(s string) OwnerStatus {
	switch s {
	case OwnerStatusAlive:
		return OwnerAlive
	case OwnerStatusGone:
		return OwnerGone
	default:
		return OwnerUnknown
	}
}

// acquireWireRequest is the JSON payload sent by daemon-side
// NATSClient.RequestAcquire and consumed by Manager.handleAcquireMsg.
type acquireWireRequest struct {
	Bridge      string `json:"bridge,omitempty"`
	ClientID    string `json:"client_id"`
	Hostname    string `json:"hostname,omitempty"`
	VendorClass string `json:"vendor_class,omitempty"`
	HWAddr      string `json:"hwaddr,omitempty"`
	UseIfaceMAC bool   `json:"use_iface_mac,omitempty"`
	Purpose     string `json:"purpose,omitempty"`
	PoolName    string `json:"pool_name,omitempty"`
	VPCID       string `json:"vpc_id,omitempty"`
}

// acquireWireReply carries either an error string or the resulting
// Lease. Empty Error means success; Lease may still be nil if the
// caller is the vpcd's own internal short-circuit (it isn't, currently).
type acquireWireReply struct {
	Error string     `json:"error,omitempty"`
	Lease *wireLease `json:"lease,omitempty"`
}

// releaseWireRequest carries either a ClientID or (PoolName, IP) for reverse-
// lookup. Daemon-side callers use IP (that's all Allocator.Release provides);
// in-process callers use ClientID.
type releaseWireRequest struct {
	ClientID string `json:"client_id,omitempty"`
	PoolName string `json:"pool_name,omitempty"`
	IP       string `json:"ip,omitempty"`
}

// Code is machine-readable and lets callers branch on a condition without
// matching the human-readable Error text. A responder that predates Code omits
// it, which degrades to a plain error rather than misreporting one.
type releaseWireReply struct {
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// drainWireReply reports how many leases the responding vpcd released. The
// drain request itself carries no body — the manager drains its entire
// per-AZ lease store, so there are no parameters.
type drainWireReply struct {
	Released int    `json:"released"`
	Error    string `json:"error,omitempty"`
}

// wireLease is Lease in JSON-friendly form: dotted-quad IPs, MAC string,
// durations as nanoseconds.
type wireLease struct {
	Bridge        string        `json:"bridge,omitempty"`
	ClientID      string        `json:"client_id"`
	Hostname      string        `json:"hostname,omitempty"`
	VendorClass   string        `json:"vendor_class,omitempty"`
	HWAddr        string        `json:"hwaddr,omitempty"`
	UseIfaceMAC   bool          `json:"use_iface_mac,omitempty"`
	IP            string        `json:"ip,omitempty"`
	SubnetMask    string        `json:"subnet_mask,omitempty"`
	Routers       []string      `json:"routers,omitempty"`
	DNS           []string      `json:"dns,omitempty"`
	ServerID      string        `json:"server_id,omitempty"`
	AcquiredAt    time.Time     `json:"acquired"`
	LeaseDuration time.Duration `json:"lease_duration"`
	T1            time.Duration `json:"t1"`
	T2            time.Duration `json:"t2"`
	RawOffer      []byte        `json:"raw_offer,omitempty"`
	RawACK        []byte        `json:"raw_ack,omitempty"`
}

func toWireLease(l *Lease) *wireLease {
	if l == nil {
		return nil
	}
	return &wireLease{
		Bridge:        l.Bridge,
		ClientID:      l.ClientID,
		Hostname:      l.Hostname,
		VendorClass:   l.VendorClass,
		HWAddr:        hwToString(l.HWAddr),
		UseIfaceMAC:   l.UseIfaceMAC,
		IP:            ipToString(l.IP),
		SubnetMask:    maskToString(l.SubnetMask),
		Routers:       ipsToStrings(l.Routers),
		DNS:           ipsToStrings(l.DNS),
		ServerID:      ipToString(l.ServerID),
		AcquiredAt:    l.AcquiredAt,
		LeaseDuration: l.LeaseDuration,
		T1:            l.T1,
		T2:            l.T2,
		RawOffer:      l.RawOffer,
		RawACK:        l.RawACK,
	}
}

func fromWireLease(w *wireLease) (*Lease, error) {
	if w == nil {
		return nil, errors.New("nil wire lease")
	}
	l := &Lease{
		Bridge:        w.Bridge,
		ClientID:      w.ClientID,
		Hostname:      w.Hostname,
		VendorClass:   w.VendorClass,
		UseIfaceMAC:   w.UseIfaceMAC,
		AcquiredAt:    w.AcquiredAt,
		LeaseDuration: w.LeaseDuration,
		T1:            w.T1,
		T2:            w.T2,
		RawOffer:      w.RawOffer,
		RawACK:        w.RawACK,
	}
	if w.IP != "" {
		l.IP = net.ParseIP(w.IP)
	}
	if w.SubnetMask != "" {
		m, err := parseMask(w.SubnetMask)
		if err != nil {
			return nil, fmt.Errorf("subnet_mask: %w", err)
		}
		l.SubnetMask = m
	}
	for _, s := range w.Routers {
		if ip := net.ParseIP(s); ip != nil {
			l.Routers = append(l.Routers, ip)
		}
	}
	for _, s := range w.DNS {
		if ip := net.ParseIP(s); ip != nil {
			l.DNS = append(l.DNS, ip)
		}
	}
	if w.ServerID != "" {
		l.ServerID = net.ParseIP(w.ServerID)
	}
	if w.HWAddr != "" {
		hw, err := net.ParseMAC(w.HWAddr)
		if err != nil {
			return nil, fmt.Errorf("hwaddr: %w", err)
		}
		l.HWAddr = hw
	}
	return l, nil
}
