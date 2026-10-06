package host

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestRouted_EnsureUplinkPort(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl port-to-br spx-nat-ovs", []byte("br-ext\n"), nil)
	rd := newStubReader()
	rd.macs[NATTransitOVSEnd] = mustMAC(t, "02:aa:bb:cc:dd:ee")
	r.expect("ip -o link show dev "+NATTransitHostEnd,
		[]byte("5: "+NATTransitHostEnd+": <BROADCAST> mtu 1500 link/ether "+NATTransitHostMAC+" brd ff:ff:ff:ff:ff:ff"), nil)
	rd.cidrs[NATTransitHostEnd] = netip.MustParsePrefix(NATTransitGatewayCIDR)

	w := &Routed{UplinkBridge: "br-ext", Runner: r, Reader: rd}
	mac, err := w.EnsureUplinkPort(context.Background())
	if err != nil {
		t.Fatalf("EnsureUplinkPort: %v", err)
	}
	if mac.String() != "02:aa:bb:cc:dd:ee" {
		t.Errorf("got MAC %q, want 02:aa:bb:cc:dd:ee", mac)
	}
	if w.UplinkMode() != UplinkModeRouted {
		t.Errorf("UplinkMode = %v, want routed", w.UplinkMode())
	}
	if r.called("ip link set " + NATTransitHostEnd + " address") {
		t.Error("a host end already on the shared MAC must not be rewritten")
	}
}

// OVN keeps one MAC binding for the transit nexthop across the whole cluster,
// so a node whose veth kept its random MAC receives none of the egress that
// binding points at. Measured on three nodes: two of three guests were
// unreachable until every host end carried the same address.
func TestEnsureTransitHostMAC(t *testing.T) {
	for _, tc := range []struct {
		name      string
		have      string
		wantWrite bool
	}{
		{"a veth made before the MAC was pinned", "ee:81:c7:35:f7:b9", true},
		{"already on the shared address", NATTransitHostMAC, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStubRunner()
			r.expect("ip -o link show dev "+NATTransitHostEnd, linkShow(tc.have), nil)
			r.expect("ip link set "+NATTransitHostEnd+" address", nil, nil)

			if err := EnsureTransitHostMAC(context.Background(), r); err != nil {
				t.Fatalf("EnsureTransitHostMAC: %v", err)
			}
			wrote := r.called("ip link set " + NATTransitHostEnd + " address " + NATTransitHostMAC)
			if wrote != tc.wantWrite {
				t.Errorf("wrote MAC = %v, want %v (calls: %v)", wrote, tc.wantWrite, r.calls)
			}
		})
	}
}

// A host end that cannot be given the shared MAC is a node nothing can reach
// through, so continuing would hide it behind a datapath failure with no
// control-plane signal at all.
func TestEnsureTransitHostMACFailureSurfaces(t *testing.T) {
	r := newStubRunner()
	r.expect("ip -o link show dev "+NATTransitHostEnd, linkShow("ee:81:c7:35:f7:b9"), nil)
	r.expect("ip link set "+NATTransitHostEnd+" address", []byte("RTNETLINK: busy"), errors.New("exit 2"))

	err := EnsureTransitHostMAC(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), NATTransitHostMAC) {
		t.Fatalf("expected a MAC-set failure naming %s, got: %v", NATTransitHostMAC, err)
	}
}

func linkShow(mac string) []byte {
	return []byte("5: " + NATTransitHostEnd + ": <BROADCAST,MULTICAST,UP> mtu 1500 link/ether " +
		mac + " brd ff:ff:ff:ff:ff:ff\n")
}

func TestRouted_EnsureUplinkPort_WrongBridge(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl port-to-br spx-nat-ovs", []byte("br-other\n"), nil)
	w := &Routed{UplinkBridge: "br-ext", Runner: r, Reader: newStubReader()}
	_, err := w.EnsureUplinkPort(context.Background())
	if err == nil || !strings.Contains(err.Error(), "br-other") {
		t.Fatalf("expected bridge mismatch error, got: %v", err)
	}
}

func TestRouted_EnsureUplinkPort_WrongTransitIP(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl port-to-br spx-nat-ovs", []byte("br-ext\n"), nil)
	rd := newStubReader()
	rd.cidrs[NATTransitHostEnd] = netip.MustParsePrefix("192.0.2.1/24")
	w := &Routed{UplinkBridge: "br-ext", Runner: r, Reader: rd}
	_, err := w.EnsureUplinkPort(context.Background())
	if err == nil || !strings.Contains(err.Error(), NATTransitGatewayCIDR) {
		t.Fatalf("expected transit CIDR mismatch error, got: %v", err)
	}
}

func TestRouted_EnsureUplinkPort_MissingHostEnd(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl port-to-br spx-nat-ovs", []byte("br-ext\n"), nil)
	w := &Routed{UplinkBridge: "br-ext", Runner: r, Reader: newStubReader()}
	_, err := w.EnsureUplinkPort(context.Background())
	if err == nil || !strings.Contains(err.Error(), NATTransitHostEnd) {
		t.Fatalf("expected missing host end error, got: %v", err)
	}
}

func TestRouted_ExternalCIDR(t *testing.T) {
	rd := newStubReader()
	want := netip.MustParsePrefix(NATTransitGatewayCIDR)
	rd.cidrs[NATTransitHostEnd] = want
	w := &Routed{UplinkBridge: "br-ext", Reader: rd}
	got, err := w.ExternalCIDR(context.Background())
	if err != nil {
		t.Fatalf("ExternalCIDR: %v", err)
	}
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}
