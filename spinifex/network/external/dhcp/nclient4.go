package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// NClient4Client is the production DHCP client (AF_PACKET per call, no
// long-lived socket). Single-shot: Manager.acquireWithBackoff owns retries
// so budget arithmetic stays in one place.
type NClient4Client struct {
	timeout time.Duration
}

const nclient4Retries = 1

var _ Client = (*NClient4Client)(nil)

// NewNClient4 creates an NClient4Client. timeout is the nclient4-internal
// socket read deadline used as a safety net when the caller supplies no
// context deadline.
func NewNClient4(timeout time.Duration) *NClient4Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &NClient4Client{timeout: timeout}
}

// socketTimeoutGrace keeps the socket deadline strictly behind the caller's, so
// ctx.Done() is what ends a timed-out attempt. Matching the two exactly makes
// them expire together and the reported error becomes a coin toss between
// context.DeadlineExceeded and nclient4's ErrNoResponse, which reads as a
// packet-matching fault rather than the plain timeout it is.
const socketTimeoutGrace = time.Second

// socketTimeout is the read deadline handed to nclient4 for one DORA. It must
// track the caller's deadline: nclient4 races its own deadline against ctx and
// the shorter one ends the attempt, so a fixed value below the caller's budget
// silently truncates every window in Manager's retransmission schedule.
func (c *NClient4Client) socketTimeout(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return c.timeout
	}
	// A deadline already in the past is left to ctx.Done(); nclient4 rejects a
	// non-positive timeout, so keep the fallback rather than pass it through.
	if remaining := time.Until(deadline); remaining > 0 {
		return remaining + socketTimeoutGrace
	}
	return c.timeout
}

func (c *NClient4Client) Acquire(ctx context.Context, req AcquireRequest) (*Lease, error) {
	if req.Bridge == "" {
		return nil, fmt.Errorf("dhcp acquire: bridge is required")
	}
	switch {
	case req.UseIfaceMAC:
		// The uplink drops foreign source MACs (WiFi/WWAN), so lease with the
		// interface's own MAC; option 61 keeps leases apart on sane servers.
		iface, err := net.InterfaceByName(req.Bridge)
		if err != nil {
			return nil, fmt.Errorf("dhcp acquire: interface MAC for %s: %w", req.Bridge, err)
		}
		if len(iface.HardwareAddr) == 0 {
			return nil, fmt.Errorf("dhcp acquire: interface %s has no MAC", req.Bridge)
		}
		req.HWAddr = iface.HardwareAddr
	case len(req.HWAddr) == 0:
		if req.ClientID == "" {
			return nil, fmt.Errorf("dhcp acquire: client_id or hw_addr is required")
		}
		hw, err := DeriveMAC(req.ClientID)
		if err != nil {
			return nil, fmt.Errorf("dhcp acquire: derive hw_addr: %w", err)
		}
		req.HWAddr = hw
	}

	releasePromisc, err := enableBridgePromisc(req.Bridge)
	if err != nil {
		slog.Warn("dhcp acquire: continuing without IFF_PROMISC", "bridge", req.Bridge, "err", err)
	} else {
		defer func() {
			if rerr := releasePromisc(); rerr != nil {
				slog.Warn("dhcp acquire: release promisc", "bridge", req.Bridge, "err", rerr)
			}
		}()
	}

	client, err := nclient4.New(req.Bridge,
		nclient4.WithHWAddr(req.HWAddr),
		nclient4.WithTimeout(c.socketTimeout(ctx)),
		nclient4.WithRetry(nclient4Retries),
	)
	if err != nil {
		return nil, fmt.Errorf("open nclient4 on %s: %w", req.Bridge, err)
	}
	defer func() { _ = client.Close() }()

	// Broadcast flag forces ff:ff:ff:ff:ff:ff destination — without it the
	// server unicasts to the derived chaddr MAC which the NIC drops in hw.
	mods := append(IdentityModifiers(req.ClientID, req.Hostname, req.VendorClass, req.HWAddr), dhcpv4.WithBroadcast(true))
	lease, err := client.Request(ctx, mods...)
	if err != nil {
		return nil, fmt.Errorf("dhcp DORA on %s (client=%s): %w", req.Bridge, req.ClientID, err)
	}
	return leaseFromNClient4(req, lease), nil
}

func (c *NClient4Client) Renew(ctx context.Context, lease *Lease) (*Lease, error) {
	if lease == nil {
		return nil, fmt.Errorf("dhcp renew: lease is nil")
	}
	nclient4Lease, err := reconstructNClient4Lease(lease)
	if err != nil {
		return nil, fmt.Errorf("dhcp renew: %w", err)
	}

	releasePromisc, err := enableBridgePromisc(lease.Bridge)
	if err != nil {
		slog.Warn("dhcp renew: continuing without IFF_PROMISC", "bridge", lease.Bridge, "err", err)
	} else {
		defer func() {
			if rerr := releasePromisc(); rerr != nil {
				slog.Warn("dhcp renew: release promisc", "bridge", lease.Bridge, "err", rerr)
			}
		}()
	}

	client, err := nclient4.New(lease.Bridge,
		nclient4.WithHWAddr(lease.HWAddr),
		nclient4.WithTimeout(c.socketTimeout(ctx)),
		nclient4.WithRetry(nclient4Retries),
	)
	if err != nil {
		return nil, fmt.Errorf("open nclient4 on %s for renew: %w", lease.Bridge, err)
	}
	defer func() { _ = client.Close() }()

	renewed, err := client.Renew(ctx, nclient4Lease,
		IdentityModifiers(lease.ClientID, lease.Hostname, lease.VendorClass, lease.HWAddr)...)
	if err != nil {
		return nil, fmt.Errorf("dhcp renew on %s (client=%s): %w", lease.Bridge, lease.ClientID, err)
	}

	return leaseFromNClient4(AcquireRequest{
		Bridge:      lease.Bridge,
		ClientID:    lease.ClientID,
		Hostname:    lease.Hostname,
		VendorClass: lease.VendorClass,
		HWAddr:      lease.HWAddr,
		UseIfaceMAC: lease.UseIfaceMAC,
	}, renewed), nil
}

func (c *NClient4Client) Release(_ context.Context, lease *Lease) error {
	if lease == nil {
		return nil
	}
	nclient4Lease, err := reconstructNClient4Lease(lease)
	if err != nil {
		return fmt.Errorf("dhcp release: %w", err)
	}

	releasePromisc, err := enableBridgePromisc(lease.Bridge)
	if err != nil {
		slog.Warn("dhcp release: continuing without IFF_PROMISC", "bridge", lease.Bridge, "err", err)
	} else {
		defer func() {
			if rerr := releasePromisc(); rerr != nil {
				slog.Warn("dhcp release: release promisc", "bridge", lease.Bridge, "err", rerr)
			}
		}()
	}

	client, err := nclient4.New(lease.Bridge,
		nclient4.WithHWAddr(lease.HWAddr),
		nclient4.WithTimeout(c.timeout),
	)
	if err != nil {
		return fmt.Errorf("open nclient4 on %s for release: %w", lease.Bridge, err)
	}
	defer func() { _ = client.Close() }()

	// The server matches a RELEASE to a lease by client-id, so this must encode
	// identically to the DISCOVER/REQUEST that took the lease out.
	if err := client.Release(nclient4Lease,
		dhcpv4.WithOption(clientIDOption(lease.ClientID, lease.HWAddr))); err != nil {
		return fmt.Errorf("dhcp release on %s (client=%s): %w", lease.Bridge, lease.ClientID, err)
	}
	return nil
}

// DeriveMAC returns the deterministic 02:xx:xx:xx:xx:xx chaddr for clientID,
// using the same HashMAC scheme as OVN router/port MACs so leases are legible
// in upstream dnsmasq logs.
func DeriveMAC(clientID string) (net.HardwareAddr, error) {
	hw, err := net.ParseMAC(utils.HashMAC(clientID))
	if err != nil {
		return nil, fmt.Errorf("derive mac for client-id %q: %w", clientID, err)
	}
	return hw, nil
}

// defaultVendorClass marks a lease as ours in the upstream lease table when the
// caller supplies nothing more specific.
const defaultVendorClass = "mulga-spinifex"

// clientIDOption encodes option 61 as RFC 2132 §9.14 requires: a type byte
// followed by the identifier. Ethernet (type 1) paired with the chaddr lets the
// server bind the lease to a hardware address, so it lists with a real MAC.
//
// Sending a bare string instead makes the server read its first byte as the
// hardware type — "dhcp-gw-lrp-vpc-x" arrives as hardware-type 100 ('d') with
// the identifier truncated to "hcp-gw-lrp-vpc-x" — and since that is not
// Ethernet, no hardware address is recorded against the lease at all.
func clientIDOption(clientID string, hwAddr net.HardwareAddr) dhcpv4.Option {
	if len(hwAddr) == 6 {
		return dhcpv4.OptClientIdentifier(append([]byte{0x01}, hwAddr...))
	}
	// Type 0 marks an identifier that is not a hardware address (RFC 4361 §6.1).
	return dhcpv4.OptClientIdentifier(append([]byte{0x00}, clientID...))
}

// IdentityModifiers builds the three identifying DHCP options set on every
// outbound message: option 61 (client-id), option 12 (hostname), option 60
// (vendor class). Hostname and vendor class carry the human-readable identity,
// since option 61 encodes the chaddr rather than the client-id string; without
// them a lease is unattributable in the upstream table.
//
// Exported so the dhcptest probe puts the same bytes on the wire as vpcd does;
// a probe that builds its own options can only confirm its own behaviour.
func IdentityModifiers(clientID, hostname, vendorClass string, hwAddr net.HardwareAddr) []dhcpv4.Modifier {
	var mods []dhcpv4.Modifier
	if clientID != "" {
		mods = append(mods, dhcpv4.WithOption(clientIDOption(clientID, hwAddr)))
	}
	if hostname == "" {
		hostname = clientID
	}
	if hostname != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptHostName(hostname)))
	}
	if vendorClass == "" {
		vendorClass = defaultVendorClass
	}
	mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier(vendorClass)))
	return mods
}

func leaseFromNClient4(req AcquireRequest, in *nclient4.Lease) *Lease {
	ack := in.ACK
	leaseTime := ack.IPAddressLeaseTime(24 * time.Hour)
	return &Lease{
		Bridge:        req.Bridge,
		ClientID:      req.ClientID,
		Hostname:      req.Hostname,
		VendorClass:   req.VendorClass,
		HWAddr:        req.HWAddr,
		UseIfaceMAC:   req.UseIfaceMAC,
		IP:            ack.YourIPAddr,
		SubnetMask:    ack.SubnetMask(),
		Routers:       ack.Router(),
		DNS:           ack.DNS(),
		ServerID:      ack.ServerIdentifier(),
		AcquiredAt:    in.CreationTime,
		LeaseDuration: leaseTime,
		T1:            ack.IPAddressRenewalTime(leaseTime / 2),
		T2:            ack.IPAddressRebindingTime(leaseTime * 7 / 8),
		RawOffer:      in.Offer.ToBytes(),
		RawACK:        ack.ToBytes(),
	}
}

func reconstructNClient4Lease(l *Lease) (*nclient4.Lease, error) {
	if len(l.RawOffer) == 0 || len(l.RawACK) == 0 {
		return nil, fmt.Errorf("lease is missing raw offer/ack bytes; renewal/release not possible")
	}
	offer, err := dhcpv4.FromBytes(l.RawOffer)
	if err != nil {
		return nil, fmt.Errorf("parse stored offer: %w", err)
	}
	ack, err := dhcpv4.FromBytes(l.RawACK)
	if err != nil {
		return nil, fmt.Errorf("parse stored ack: %w", err)
	}
	return &nclient4.Lease{
		Offer:        offer,
		ACK:          ack,
		CreationTime: l.AcquiredAt,
	}, nil
}
