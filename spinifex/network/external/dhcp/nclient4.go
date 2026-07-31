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
// long-lived socket). Manager.acquireWithBackoff owns the outer schedule of
// whole DORA exchanges; this type owns the retransmissions inside one exchange.
type NClient4Client struct {
	timeout time.Duration
}

// nclient4InitialRead is the read deadline for the FIRST transmission of an
// exchange. nclient4's retryFn doubles it per retransmission, so the ladder is
// 1s, 2s, 4s, … — RFC 2131 §4.1 behaviour.
//
// Retransmitting inside the exchange is the whole point, and it is not the same
// as Manager retrying: Manager builds a new client and therefore a new xid each
// attempt, so a server that answers late, or that ignores a first-contact
// DISCOVER while it probes the candidate address, has its OFFER dropped as
// belonging to an unknown transaction. Observed upstream behaviour is exactly
// that — the first DISCOVER from an unseen MAC goes unanswered and a
// retransmission is answered within ~500ms — so a client that sends one packet
// and waits out the window never completes DORA at all.
const nclient4InitialRead = time.Second

// nclient4Tries is the number of transmissions per exchange. Six spans a 63s
// ladder, longer than the widest window Manager schedules, so an attempt always
// ends on the caller's context rather than on an exhausted ladder.
const nclient4Tries = 6

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

// retransmitLadder is the wall-clock span of tries transmissions that start at
// initial and double each time. The caller's budget must sit inside this span,
// otherwise the ladder runs out first and the exchange ends early with time
// still on the clock.
func retransmitLadder(initial time.Duration, tries int) time.Duration {
	var total, step time.Duration = 0, initial
	for range tries {
		total += step
		step *= 2
	}
	return total
}

// socketTimeout is the read deadline for the first transmission of an exchange.
// It is deliberately short so the retransmissions above actually happen: the
// budget is spent on repeated DISCOVERs under one xid, not on a single silent
// wait. nclient4 races this against ctx and the shorter wins, so it is clamped
// to whatever the caller has left.
func (c *NClient4Client) socketTimeout(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nclient4InitialRead
	}
	// A deadline already in the past is left to ctx.Done(); nclient4 rejects a
	// non-positive timeout, so keep the fallback rather than pass it through.
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nclient4InitialRead
	}
	return min(remaining, nclient4InitialRead)
}

// resolveHWAddr picks the chaddr for an exchange: the interface's own MAC when
// the pool leases under it, otherwise the deterministic per-client-id MAC.
//
// A stored address that is empty or the wrong width is repaired rather than
// passed through. nclient4 would put all zeros in chaddr and clientIDOption
// would fall back to its untyped form, so the lease lands upstream with no
// hardware address against it at all — the exact outcome option 61 was fixed to
// stop, reached by a different route.
func resolveHWAddr(bridge, clientID string, hwAddr net.HardwareAddr, useIfaceMAC bool) (net.HardwareAddr, error) {
	if useIfaceMAC {
		// The uplink drops foreign source MACs (WiFi/WWAN), so lease with the
		// interface's own MAC; option 61 keeps leases apart on sane servers.
		iface, err := net.InterfaceByName(bridge)
		if err != nil {
			return nil, fmt.Errorf("interface MAC for %s: %w", bridge, err)
		}
		if len(iface.HardwareAddr) == 0 {
			return nil, fmt.Errorf("interface %s has no MAC", bridge)
		}
		return iface.HardwareAddr, nil
	}
	// All-zero is six bytes wide, so a width check alone lets it through — and it
	// is the value that produces the unattributable lease in the first place.
	if len(hwAddr) == 6 && !isZeroMAC(hwAddr) {
		return hwAddr, nil
	}
	if clientID == "" {
		return nil, fmt.Errorf("client_id or hw_addr is required")
	}
	hw, err := DeriveMAC(clientID)
	if err != nil {
		return nil, fmt.Errorf("derive hw_addr: %w", err)
	}
	return hw, nil
}

// releaseHWAddr picks the chaddr for a RELEASE. A server matches a RELEASE to a
// binding on chaddr, so the only address that can work is the one the lease was
// taken with, which the record already carries.
//
// This differs from resolveHWAddr only for a UseIfaceMAC lease whose interface
// MAC has changed since: re-resolving live would send an address the binding
// never had, and the address would stay held with the server saying nothing.
func releaseHWAddr(lease *Lease) (net.HardwareAddr, error) {
	if len(lease.HWAddr) == 6 && !isZeroMAC(lease.HWAddr) {
		return lease.HWAddr, nil
	}
	return resolveHWAddr(lease.Bridge, lease.ClientID, lease.HWAddr, lease.UseIfaceMAC)
}

// isZeroMAC reports whether hw is all zeros, the placeholder a lease carries
// when no hardware address was ever recorded for it.
func isZeroMAC(hw net.HardwareAddr) bool {
	for _, b := range hw {
		if b != 0 {
			return false
		}
	}
	return true
}

func (c *NClient4Client) Acquire(ctx context.Context, req AcquireRequest) (*Lease, error) {
	if req.Bridge == "" {
		return nil, fmt.Errorf("dhcp acquire: bridge is required")
	}
	hw, err := resolveHWAddr(req.Bridge, req.ClientID, req.HWAddr, req.UseIfaceMAC)
	if err != nil {
		return nil, fmt.Errorf("dhcp acquire: %w", err)
	}
	req.HWAddr = hw

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
		nclient4.WithRetry(nclient4Tries),
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
	// A lease persisted before the chaddr was carried has none to renew under.
	hwAddr, err := resolveHWAddr(lease.Bridge, lease.ClientID, lease.HWAddr, lease.UseIfaceMAC)
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
		nclient4.WithHWAddr(hwAddr),
		nclient4.WithTimeout(c.socketTimeout(ctx)),
		nclient4.WithRetry(nclient4Tries),
	)
	if err != nil {
		return nil, fmt.Errorf("open nclient4 on %s for renew: %w", lease.Bridge, err)
	}
	defer func() { _ = client.Close() }()

	renewed, err := client.Renew(ctx, nclient4Lease,
		IdentityModifiers(lease.ClientID, lease.Hostname, lease.VendorClass, hwAddr)...)
	if err != nil {
		return nil, fmt.Errorf("dhcp renew on %s (client=%s): %w", lease.Bridge, lease.ClientID, err)
	}

	return leaseFromNClient4(AcquireRequest{
		Bridge:      lease.Bridge,
		ClientID:    lease.ClientID,
		Hostname:    lease.Hostname,
		VendorClass: lease.VendorClass,
		HWAddr:      hwAddr,
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
	// Must match what took the lease out, so it comes from the lease itself.
	hwAddr, err := releaseHWAddr(lease)
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
		nclient4.WithHWAddr(hwAddr),
		nclient4.WithTimeout(c.timeout),
	)
	if err != nil {
		return fmt.Errorf("open nclient4 on %s for release: %w", lease.Bridge, err)
	}
	defer func() { _ = client.Close() }()

	// The server matches a RELEASE to a lease by client-id, so this must encode
	// identically to the DISCOVER/REQUEST that took the lease out.
	if err := client.Release(nclient4Lease,
		dhcpv4.WithOption(clientIDOption(lease.ClientID, hwAddr))); err != nil {
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
