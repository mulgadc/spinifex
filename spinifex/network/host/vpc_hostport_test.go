package host

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

func testVPCHostPort() VPCHostPort {
	return VPCHostPort{
		Name:    VPCHostPortName("eni-0123456789abcdef0"),
		IfaceID: "eni-0123456789abcdef0",
		MAC:     "02:0a:01:23:45:67",
		Addr:    netip.MustParsePrefix("10.246.29.9/24"),
	}
}

func TestInstallVPCHostPortValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		mut  func(*VPCHostPort)
	}{
		{"no name", "Name", func(d *VPCHostPort) { d.Name = "" }},
		{"no iface id", "IfaceID", func(d *VPCHostPort) { d.IfaceID = "" }},
		{"no mac", "MAC", func(d *VPCHostPort) { d.MAC = "" }},
		{"no addr", "Addr", func(d *VPCHostPort) { d.Addr = netip.Prefix{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubRunner()
			d := testVPCHostPort()
			tc.mut(&d)
			if err := installVPCHostPort(context.Background(), s, d); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %s validation error, got %v", tc.want, err)
			}
			if len(s.calls) != 0 {
				t.Errorf("validation must fail before issuing commands; calls: %v", s.calls)
			}
		})
	}
}

func TestInstallVPCHostPort(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip", nil, nil)

	d := testVPCHostPort()
	if err := installVPCHostPort(context.Background(), s, d); err != nil {
		t.Fatalf("installVPCHostPort: %v", err)
	}

	want := []string{
		// An internal port on br-int carrying the OVN binding, so ovn-controller
		// binds the ENI's LSP to it exactly as it would to a guest tap.
		"ovs-vsctl --may-exist add-port br-int " + d.Name + " -- set Interface " + d.Name +
			" type=internal external_ids:iface-id=" + d.IfaceID + " external_ids:attached-mac=" + d.MAC,
		"ip link set " + d.Name + " address " + d.MAC,
		"ip addr replace 10.246.29.9/24 dev " + d.Name,
		"ip link set " + d.Name + " up",
	}
	for _, w := range want {
		if !s.called(w) {
			t.Errorf("missing command:\n  %q\ncalls: %v", w, s.calls)
		}
	}
}

// The subnet prefix length is what installs the connected route covering every
// serving VM. A /32 would address the port and still leave the daemon unable to
// reach anything else in the subnet, which is the whole point of the port.
func TestInstallVPCHostPortKeepsSubnetPrefixLength(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip", nil, nil)

	d := testVPCHostPort()
	if err := installVPCHostPort(context.Background(), s, d); err != nil {
		t.Fatalf("installVPCHostPort: %v", err)
	}
	if s.called("ip addr replace 10.246.29.9/32") {
		t.Errorf("host address installed as a /32; calls: %v", s.calls)
	}
	if !s.called("ip addr replace 10.246.29.9/24") {
		t.Errorf("host address not installed at the subnet prefix length; calls: %v", s.calls)
	}
}

// The address must not be masked to the network address on the way through:
// 10.246.29.9/24 has to stay .9, not become .0.
func TestInstallVPCHostPortDoesNotMaskHostAddress(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip", nil, nil)

	if err := installVPCHostPort(context.Background(), s, testVPCHostPort()); err != nil {
		t.Fatalf("installVPCHostPort: %v", err)
	}
	if s.called("ip addr replace 10.246.29.0/24") {
		t.Errorf("host address masked to the network address; calls: %v", s.calls)
	}
}

// A replaced appliance leaves its vhp- port behind at the reused subnet IP.
// Installing the new port must prune that stale namesake first, or two devs
// claim one address and the kernel can route to the dead one.
func TestInstallVPCHostPortPrunesCollidingPort(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	// Distinct prefixes so the stub matches deterministically: the readiness
	// probe (`ip -4 -o addr show`) reports a stale vhp- namesake at our addr.
	s.expect("ip -4", []byte("7: vhp-stalest    inet 10.246.29.9/24 brd 10.246.29.255 scope global vhp-stalest\\    valid_lft forever preferred_lft forever\n"), nil)
	s.expect("ip link", nil, nil)
	s.expect("ip addr", nil, nil)

	d := testVPCHostPort()
	if err := installVPCHostPort(context.Background(), s, d); err != nil {
		t.Fatalf("installVPCHostPort: %v", err)
	}
	if !s.called("ovs-vsctl --if-exists del-port br-int vhp-stalest") {
		t.Errorf("stale colliding vhp port was not pruned; calls: %v", s.calls)
	}
	if s.called("ovs-vsctl --if-exists del-port br-int " + d.Name) {
		t.Errorf("must not prune the port being installed; calls: %v", s.calls)
	}
}

// Only our own vhp- ports are pruned: the port being installed (post-reboot it
// already holds the addr) and any foreign dev sharing it must be left alone.
func TestPruneCollidingVPCHostPortsLeavesKeepAndForeign(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	keep := VPCHostPortName("eni-0123456789abcdef0")
	s.expect("ip -4", []byte(
		"3: "+keep+"    inet 10.246.29.9/24 scope global "+keep+"\n"+
			"9: eth0    inet 10.246.29.9/24 scope global eth0\n"), nil)

	if err := pruneCollidingVPCHostPorts(context.Background(), s, keep, netip.MustParsePrefix("10.246.29.9/24")); err != nil {
		t.Fatalf("pruneCollidingVPCHostPorts: %v", err)
	}
	if s.called("ovs-vsctl --if-exists del-port") {
		t.Errorf("nothing should be pruned (keep + foreign dev); calls: %v", s.calls)
	}
}

func TestRemoveVPCHostPort(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)

	name := VPCHostPortName("eni-0123456789abcdef0")
	if err := removeVPCHostPort(context.Background(), s, name); err != nil {
		t.Fatalf("removeVPCHostPort: %v", err)
	}
	// --if-exists so a re-run, or an ENI that never had a port, is not an error.
	if !s.called("ovs-vsctl --if-exists del-port br-int " + name) {
		t.Errorf("port not deleted from br-int; calls: %v", s.calls)
	}
}

func TestRemoveVPCHostPortRejectsEmptyName(t *testing.T) {
	s := newStubRunner()
	if err := removeVPCHostPort(context.Background(), s, ""); err == nil {
		t.Fatal("expected an error for an empty port name")
	}
	if len(s.calls) != 0 {
		t.Errorf("empty name must issue no commands; calls: %v", s.calls)
	}
}

// IFNAMSIZ is 15 chars and the kernel rejects anything longer, so the name has
// to stay short no matter how long the ENI ID is.
func TestVPCHostPortNameWithinIFNAMSIZ(t *testing.T) {
	for _, eni := range []string{"eni-1", "eni-0123456789abcdef0", strings.Repeat("e", 200)} {
		if got := VPCHostPortName(eni); len(got) > 15 {
			t.Errorf("VPCHostPortName(%q) = %q, %d chars, over the 15-char limit", eni, got, len(got))
		}
	}
}

// Distinct ENIs must not collide on one port name, or two daemons' ports would
// fight over the same netdev.
func TestVPCHostPortNameDistinctPerENI(t *testing.T) {
	a := VPCHostPortName("eni-aaaaaaaaaaaaaaaaa")
	b := VPCHostPortName("eni-bbbbbbbbbbbbbbbbb")
	if a == b {
		t.Errorf("distinct ENIs collided on port name %q", a)
	}
}
