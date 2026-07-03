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
		nclient4.WithTimeout(c.timeout),
		nclient4.WithRetry(nclient4Retries),
	)
	if err != nil {
		return nil, fmt.Errorf("open nclient4 on %s: %w", req.Bridge, err)
	}
	defer func() { _ = client.Close() }()

	// Broadcast flag forces ff:ff:ff:ff:ff:ff destination — without it the
	// server unicasts to the derived chaddr MAC which the NIC drops in hw.
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
		nclient4.WithTimeout(c.timeout),
		nclient4.WithRetry(nclient4Retries),
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

	if err := client.Release(nclient4Lease,
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier([]byte(lease.ClientID)))); err != nil {
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
