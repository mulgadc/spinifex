// Package install performs the disk installation steps after the UI has
// collected configuration.
package install

import (
	"fmt"
	"net"
	"strings"
)

// Plane names the three traffic planes a node presents. Each plane is a role
// bound to a target, not a fixed NIC: the same three roles collapse onto one,
// two or three interfaces depending on the hardware.
type Plane string

const (
	// PlaneWAN carries public north-south traffic: ingress/egress, EIPs and
	// SNAT/DNAT. It is the OVN external bridge-mapping.
	PlaneWAN Plane = "wan"

	// PlaneLAN carries internal cluster traffic: predastore replication, the
	// NATS cluster mesh, OVN NB/SB control, Raft and S3DB.
	PlaneLAN Plane = "lan"

	// PlaneVPC carries the tenant overlay: OVN Geneve tunnels between nodes.
	// It is the ovn-encap-ip endpoint.
	PlaneVPC Plane = "vpc"
)

// Bridge returns the Linux bridge that carries this plane.
func (p Plane) Bridge() string { return "br-" + string(p) }

// NetworkRole binds one plane to a network target. An empty Interface means
// the role is folded onto the next role up the chain (vpc <- lan <- wan) and
// no bridge of its own is created.
//
// VLAN is what allows two roles to share one physical NIC while staying
// separate planes: each gets its own NIC.<vlan> subinterface and bridge. Two
// roles on the same Interface with no VLAN are the same plane, not two.
type NetworkRole struct {
	// Interface is the physical NIC this role binds to; empty means folded.
	Interface string

	// VLAN is the 802.1Q tag, or 0 for untagged.
	VLAN int

	// DHCPMode leaves addressing to the DHCP client; Address/Mask are then unset.
	DHCPMode bool

	Address string
	Mask    string

	// Gateway is only meaningful on the wan role — the other planes are
	// link-local to the rack and must not install a default route.
	Gateway string

	DNS []string

	// MTU is written as MTUBytes= on the link; 0 leaves the kernel default.
	// East-west planes typically run 9000, wan stays at 1500.
	MTU int

	// WiFiSSID and WiFiPass are set when the selected NIC is wireless.
	WiFiSSID string
	WiFiPass string
}

// Folded reports whether this role has no interface of its own and inherits
// the plane of the role above it.
func (r NetworkRole) Folded() bool { return r.Interface == "" }

// Link returns the name of the interface carrying this role's address: the
// physical NIC, or its VLAN subinterface when tagged. Empty when folded.
func (r NetworkRole) Link() string {
	if r.Folded() {
		return ""
	}
	if r.VLAN > 0 {
		return fmt.Sprintf("%s.%d", r.Interface, r.VLAN)
	}
	return r.Interface
}

// Config holds all values collected by the installer UI.
type Config struct {
	// Storage is the filesystem choice and the ordered set of disks it is
	// built from. In ext4 mode it holds exactly one disk.
	Storage DiskConfig

	// WAN is always present and can never be folded — a node needs an uplink.
	// The address lives on br-wan (a Linux bridge over the NIC), not on the
	// physical NIC itself, so OVN/OVS can attach to the bridge without
	// disrupting host connectivity.
	WAN NetworkRole

	// LAN carries internal cluster traffic. Folded onto WAN on single-NIC nodes.
	LAN NetworkRole

	// VPC carries Geneve overlay traffic and supplies ovn-encap-ip. Folded onto
	// LAN unless a node dedicates an interface or VLAN to it.
	VPC NetworkRole

	// Node identity
	Hostname string

	// SkipFormation skips spx admin init/join in firstboot; the provisioning
	// controller (e.g. bm-bootstrap.sh) owns cluster formation instead.
	SkipFormation bool

	// CA certificate (PEM), optional.
	HasCACert bool
	CACert    string

	// RootPassword is the password to set for the root account on the installed system.
	RootPassword string

	// Email is the operator's email address, used by the call-home telemetry
	// endpoint to notify of important system updates and security advisories.
	// Required on interactive installs; may be empty on headless/CI installs
	// when SPINIFEX_EMAIL is not supplied on the kernel cmdline.
	Email string

	// GPUPassthrough enables VFIO GPU passthrough on this node.
	// Set via SPINIFEX_GPU_PASSTHROUGH=1 on headless installs.
	// Passes --gpu-passthrough to `spx admin init` during firstboot.
	GPUPassthrough bool
}

// Resolve returns the role that actually carries the given plane after
// collapsing folded roles up the chain vpc <- lan <- wan, along with the plane
// it landed on. A folded vpc on a single-NIC node resolves to the wan role.
func (c *Config) Resolve(p Plane) (NetworkRole, Plane) {
	switch p {
	case PlaneVPC:
		if !c.VPC.Folded() {
			return c.VPC, PlaneVPC
		}
		return c.Resolve(PlaneLAN)
	case PlaneLAN:
		if !c.LAN.Folded() {
			return c.LAN, PlaneLAN
		}
		return c.WAN, PlaneWAN
	default:
		return c.WAN, PlaneWAN
	}
}

// PlaneAddress returns the configured address for a plane after collapsing.
// Empty when the resolved role uses DHCP, since no static address is known
// until the lease is taken.
func (c *Config) PlaneAddress(p Plane) string {
	role, _ := c.Resolve(p)
	if role.DHCPMode {
		return ""
	}
	return role.Address
}

// PlaneBridge returns the bridge carrying a plane after collapsing, so callers
// asking for the vpc bridge on a single-NIC node correctly get br-wan.
func (c *Config) PlaneBridge(p Plane) string {
	_, landed := c.Resolve(p)
	return landed.Bridge()
}

// RoleBinding pairs a plane with the role that carries it.
type RoleBinding struct {
	Plane Plane
	Role  NetworkRole
}

// Roles returns the bound roles in bridge-creation order, skipping folded ones.
// wan comes first because br-lan and br-vpc are activated manually after it.
func (c *Config) Roles() []RoleBinding {
	out := []RoleBinding{{PlaneWAN, c.WAN}}
	if !c.LAN.Folded() {
		out = append(out, RoleBinding{PlaneLAN, c.LAN})
	}
	if !c.VPC.Folded() {
		out = append(out, RoleBinding{PlaneVPC, c.VPC})
	}
	return out
}

// Validate enforces the sharing rules the UI and the headless path must both
// obey: wan always needs a target, and two roles may only share an interface
// when distinct VLAN ids keep them on separate broadcast domains.
func (c *Config) Validate() error {
	if c.WAN.Folded() {
		return fmt.Errorf("wan role must be bound to an interface")
	}

	seen := map[string]Plane{}
	for _, r := range c.Roles() {
		link := r.Role.Link()
		if prev, dup := seen[link]; dup {
			return fmt.Errorf("%s and %s both bind %s: give each a distinct VLAN id or fold one onto the other",
				prev, r.Plane, link)
		}
		seen[link] = r.Plane
		if r.Role.VLAN < 0 || r.Role.VLAN > 4094 {
			return fmt.Errorf("%s: VLAN id %d out of range (1-4094)", r.Plane, r.Role.VLAN)
		}
		if r.Role.MTU != 0 && (r.Role.MTU < 68 || r.Role.MTU > 9216) {
			return fmt.Errorf("%s: MTU %d out of range (68-9216)", r.Plane, r.Role.MTU)
		}
		// Resolvers belong to the plane that holds the default route. Accepting
		// one here and dropping it during generation would leave an unattended
		// install believing it had set a resolver that was never written.
		if r.Plane != PlaneWAN && len(r.Role.DNS) > 0 {
			return fmt.Errorf("%s: DNS servers belong to the wan plane, which holds the default route", r.Plane)
		}
		// Netgen converts the mask with net.ParseIP, so a prefix length reaches
		// it as garbage and fails the install after the disk is partitioned.
		// Catch it here, where the operator can still fix it.
		if m := strings.TrimSpace(r.Role.Mask); m != "" && !validDottedMask(m) {
			return fmt.Errorf("%s: subnet mask %q must be dotted-decimal, e.g. 255.255.255.0", r.Plane, m)
		}
	}
	return nil
}

// validDottedMask reports whether s is a contiguous dotted-decimal IPv4 subnet
// mask. Prefix-length form ("24", "/24") is deliberately not accepted: netgen
// parses the mask with net.ParseIP, so anything else fails the install itself.
func validDottedMask(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.To4() == nil {
		return false
	}
	// Size reports zero bits for a non-contiguous mask, which netgen rejects too.
	_, bits := net.IPMask(ip.To4()).Size()
	return bits != 0
}
