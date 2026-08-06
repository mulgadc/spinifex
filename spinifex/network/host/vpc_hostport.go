package host

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

// vpcHostPortPrefix tags the daemon's own OVS internal ports on br-int. "vhp-"
// plus the 8-char short ENI is 12 chars, inside the 15-char IFNAMSIZ limit —
// the same naming discipline the per-tap IMDS ports follow.
const vpcHostPortPrefix = "vhp-"

// VPCHostPortName returns the host-side port name for an ENI the daemon itself
// owns, rather than one belonging to a guest.
func VPCHostPortName(eniID string) string { return vpcHostPortPrefix + shortENIID(eniID) }

// VPCHostPort describes one host-side port into a managed VPC subnet: an OVS
// internal port on br-int bound to a real ENI, giving the host a routed
// presence in a network that otherwise lives entirely inside OVN. Addr is the
// ENI's own address carried at the SUBNET's prefix length, which is what
// installs the connected route for the rest of the subnet.
type VPCHostPort struct {
	Name    string
	IfaceID string
	MAC     string
	Addr    netip.Prefix
}

func (d VPCHostPort) validate() error {
	switch {
	case d.Name == "":
		return fmt.Errorf("VPCHostPort: Name required")
	case d.IfaceID == "":
		return fmt.Errorf("VPCHostPort: IfaceID required")
	case d.MAC == "":
		return fmt.Errorf("VPCHostPort: MAC required")
	case !d.Addr.IsValid():
		return fmt.Errorf("VPCHostPort: Addr required")
	}
	return nil
}

// EnsureVPCHostPort installs the daemon's own routed port into a managed VPC
// subnet. addr is CIDR notation carrying the ENI address at the subnet's
// prefix length. Idempotent, so it also re-establishes the port after a host
// reboot without the caller special-casing that.
func (p *OVSPlumber) EnsureVPCHostPort(eniID, mac, addr string) error {
	prefix, err := netip.ParsePrefix(addr)
	if err != nil {
		return fmt.Errorf("VPC host port for %s: parse address %q: %w", eniID, addr, err)
	}
	return installVPCHostPort(context.Background(), NewExecRunner(), VPCHostPort{
		Name:    VPCHostPortName(eniID),
		IfaceID: vm.OVSIfaceID(eniID),
		MAC:     mac,
		Addr:    prefix,
	})
}

// RemoveVPCHostPort tears down the port installed by EnsureVPCHostPort.
// Idempotent, so it is safe for an ENI that never had a port.
func (p *OVSPlumber) RemoveVPCHostPort(eniID string) error {
	return removeVPCHostPort(context.Background(), NewExecRunner(), VPCHostPortName(eniID))
}

// installVPCHostPort creates the internal port, binds it to the ENI's logical
// switch port, and gives it the ENI's MAC and address. ovn-controller binds the
// LSP by iface-id exactly as it binds a guest tap, so the ENI (and therefore
// its LSP) must already exist when this runs.
func installVPCHostPort(ctx context.Context, r Runner, d VPCHostPort) error {
	if err := d.validate(); err != nil {
		return err
	}
	if _, err := r.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", "br-int", d.Name,
		"--", "set", "Interface", d.Name, "type=internal",
		"external_ids:iface-id="+d.IfaceID, "external_ids:attached-mac="+d.MAC); err != nil {
		return fmt.Errorf("create VPC host port %s on br-int: %w", d.Name, err)
	}
	if _, err := r.Run(ctx, "ip", "link", "set", d.Name, "address", d.MAC); err != nil {
		return fmt.Errorf("set VPC host port %s MAC: %w", d.Name, err)
	}
	// `replace` rather than `add`: OVS conf.db survives a host reboot but the
	// address on the netdev does not, so a re-run meets either state.
	if _, err := r.Run(ctx, "ip", "addr", "replace", d.Addr.String(), "dev", d.Name); err != nil {
		return fmt.Errorf("add %s to VPC host port %s: %w", d.Addr, d.Name, err)
	}
	if _, err := r.Run(ctx, "ip", "link", "set", d.Name, "up"); err != nil {
		return fmt.Errorf("bring up VPC host port %s: %w", d.Name, err)
	}
	slog.Info("VPC host port ready", "port", d.Name, "iface_id", d.IfaceID, "addr", d.Addr)
	return nil
}

// removeVPCHostPort deletes the port, which takes its address with it.
func removeVPCHostPort(ctx context.Context, r Runner, name string) error {
	if name == "" {
		return fmt.Errorf("removeVPCHostPort: name required")
	}
	if _, err := r.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", "br-int", name); err != nil {
		return fmt.Errorf("delete VPC host port %s: %w", name, err)
	}
	slog.Info("VPC host port removed", "port", name)
	return nil
}
