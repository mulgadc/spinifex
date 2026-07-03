package host

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

func TestRouted_EnsureUplinkPort(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl port-to-br spx-nat-ovs", []byte("br-ext\n"), nil)
	rd := newStubReader()
	rd.macs[NATTransitOVSEnd] = mustMAC(t, "02:aa:bb:cc:dd:ee")
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
