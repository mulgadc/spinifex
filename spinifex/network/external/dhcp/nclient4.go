package dhcp

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// NClient4Client is the production DHCP client backed by
// github.com/insomniacslk/dhcp/dhcpv4/nclient4. Each Acquire/Renew/Release
// opens an AF_PACKET socket on the target bridge for the duration of the
// handshake and closes it when done — no long-lived per-lease process.
//
// Retransmission lives one layer up in DHCPManager.acquireWithBackoff
// (RFC 2131 §4.1). This client always does exactly one DISCOVER attempt;
// the per-attempt window is whatever the caller's context allows.
type NClient4Client struct {
	timeout time.Duration
}

var _ Client = (*NClient4Client)(nil)

// NewNClient4 creates an NClient4Client. timeout is the nclient4-internal
// socket read deadline used as a safety net when the caller supplies no
// context deadline.
func NewNClient4(timeout time.Duration) *NClient4Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &NClient4Client{timeout: timeout}
}

func (c *NClient4Client) Acquire(ctx context.Context, req AcquireRequest) (*Lease, error) {
	if req.Bridge == "" {
		return nil, fmt.Errorf("dhcp acquire: bridge is required")
	}
	if len(req.HWAddr) == 0 {
		if req.ClientID == "" {
			return nil, fmt.Errorf("dhcp acquire: client_id or hw_addr is required")
		}
		hw, err := DeriveMAC(req.ClientID)
		if err != nil {
			return nil, fmt.Errorf("dhcp acquire: derive hw_addr: %w", err)
		}
		req.HWAddr = hw
	}

	client, err := nclient4.New(req.Bridge,
		nclient4.WithHWAddr(req.HWAddr),
		nclient4.WithTimeout(c.timeout),
		nclient4.WithRetry(1),
	)
	if err != nil {
		return nil, fmt.Errorf("open nclient4 on %s: %w", req.Bridge, err)
	}
	defer func() { _ = client.Close() }()

	// Without the broadcast flag, the server sends a unicast OFFER to the
	// generated chaddr MAC. The physical NIC drops it in hardware (not its
	// MAC), so the AF_PACKET socket on the bridge never sees the frame.
	// Setting the broadcast flag forces ff:ff:ff:ff:ff:ff.
	mods := append(identityModifiers(req.ClientID, req.Hostname, req.VendorClass), dhcpv4.WithBroadcast(true))
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

	client, err := nclient4.New(lease.Bridge,
		nclient4.WithHWAddr(lease.HWAddr),
		nclient4.WithTimeout(c.timeout),
		nclient4.WithRetry(1),
	)
	if err != nil {
		return nil, fmt.Errorf("open nclient4 on %s for renew: %w", lease.Bridge, err)
	}
	defer func() { _ = client.Close() }()

	renewed, err := client.Renew(ctx, nclient4Lease,
		identityModifiers(lease.ClientID, lease.Hostname, lease.VendorClass)...)
	if err != nil {
		return nil, fmt.Errorf("dhcp renew on %s (client=%s): %w", lease.Bridge, lease.ClientID, err)
	}

	return leaseFromNClient4(AcquireRequest{
		Bridge:      lease.Bridge,
		ClientID:    lease.ClientID,
		Hostname:    lease.Hostname,
		VendorClass: lease.VendorClass,
		HWAddr:      lease.HWAddr,
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

	client, err := nclient4.New(lease.Bridge,
		nclient4.WithHWAddr(lease.HWAddr),
		nclient4.WithTimeout(c.timeout),
	)
	if err != nil {
		return fmt.Errorf("open nclient4 on %s for release: %w", lease.Bridge, err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Release(nclient4Lease,
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier([]byte(lease.ClientID)))); err != nil {
		return fmt.Errorf("dhcp release on %s (client=%s): %w", lease.Bridge, lease.ClientID, err)
	}
	return nil
}

// DeriveMAC returns the deterministic locally-administered unicast MAC
// (02:xx:xx:xx:xx:xx) used as chaddr when an allocator has no NIC of
// its own — DHCPGatewayLRPAllocator and DHCPPoolAllocator on first
// acquire. Wraps utils.HashMAC so the same 0x02-prefixed scheme used
// for OVN router/port MACs is also visible in upstream dnsmasq leases.
func DeriveMAC(clientID string) (net.HardwareAddr, error) {
	hw, err := net.ParseMAC(utils.HashMAC(clientID))
	if err != nil {
		return nil, fmt.Errorf("derive mac for client-id %q: %w", clientID, err)
	}
	return hw, nil
}

// identityModifiers builds the three identifying DHCP options set on every
// outbound message: option 61 (client-id), option 12 (hostname), option 60
// (vendor class).
func identityModifiers(clientID, hostname, vendorClass string) []dhcpv4.Modifier {
	var mods []dhcpv4.Modifier
	if clientID != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClientIdentifier([]byte(clientID))))
	}
	if hostname != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptHostName(hostname)))
	}
	if vendorClass != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier(vendorClass)))
	}
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
