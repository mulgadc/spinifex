package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
)

// stubRunner records commands and returns scripted output by prefix.
type stubRunner struct {
	mu    sync.Mutex
	calls []string
	resp  map[string]stubResp
}

type stubResp struct {
	out []byte
	err error
}

func newStubRunner() *stubRunner { return &stubRunner{resp: map[string]stubResp{}} }

func (s *stubRunner) expect(prefix string, out []byte, err error) {
	s.resp[prefix] = stubResp{out: out, err: err}
}

func (s *stubRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := strings.Join(append([]string{name}, args...), " ")
	s.calls = append(s.calls, joined)
	for prefix, r := range s.resp {
		if strings.HasPrefix(joined, prefix) {
			return r.out, r.err
		}
	}
	return nil, fmt.Errorf("stubRunner: unexpected command %q", joined)
}

func (s *stubRunner) called(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

type stubReader struct {
	cidrs  map[string]netip.Prefix
	macs   map[string]net.HardwareAddr
	master map[string]string
	errs   map[string]error
}

func newStubReader() *stubReader {
	return &stubReader{
		cidrs:  map[string]netip.Prefix{},
		macs:   map[string]net.HardwareAddr{},
		master: map[string]string{},
		errs:   map[string]error{},
	}
}

func (r *stubReader) BridgeCIDR(name string) (netip.Prefix, error) {
	if err, ok := r.errs["cidr:"+name]; ok {
		return netip.Prefix{}, err
	}
	if p, ok := r.cidrs[name]; ok {
		return p, nil
	}
	return netip.Prefix{}, fmt.Errorf("%w: %q", ErrNoUplinkAddr, name)
}

func (r *stubReader) LinkMAC(name string) (net.HardwareAddr, error) {
	if err, ok := r.errs["mac:"+name]; ok {
		return nil, err
	}
	if m, ok := r.macs[name]; ok {
		return m, nil
	}
	return nil, fmt.Errorf("no MAC for %q", name)
}

func (r *stubReader) LinkMaster(name string) (string, error) {
	if err, ok := r.errs["master:"+name]; ok {
		return "", err
	}
	return r.master[name], nil
}

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("parse MAC %q: %v", s, err)
	}
	return mac
}

func TestUplinkModeString(t *testing.T) {
	cases := []struct {
		m    UplinkMode
		want string
	}{
		{UplinkModePhysical, "physical"},
		{UplinkModeVeth, "veth"},
		{UplinkModeRouted, "routed"},
		{UplinkModeUnknown, "unknown"},
		{UplinkMode(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.m.String(); got != c.want {
			t.Errorf("UplinkMode(%d).String() = %q, want %q", c.m, got, c.want)
		}
	}
}

func TestPhysical_EnsureUplinkPort(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl port-to-br enp0s3", []byte("br-ext\n"), nil)
	rd := newStubReader()
	rd.macs["enp0s3"] = mustMAC(t, "aa:bb:cc:dd:ee:ff")

	p := &Physical{
		ExternalInterface: "enp0s3",
		UplinkBridge:      "br-ext",
		Runner:            r,
		Reader:            rd,
	}
	mac, err := p.EnsureUplinkPort(context.Background())
	if err != nil {
		t.Fatalf("EnsureUplinkPort: %v", err)
	}
	if mac.String() != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("got MAC %q, want aa:bb:cc:dd:ee:ff", mac)
	}
	if p.UplinkMode() != UplinkModePhysical {
		t.Errorf("UplinkMode = %v, want physical", p.UplinkMode())
	}
}

func TestPhysical_EnsureUplinkPort_WrongBridge(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl port-to-br enp0s3", []byte("br-other\n"), nil)
	p := &Physical{
		ExternalInterface: "enp0s3",
		UplinkBridge:      "br-ext",
		Runner:            r,
		Reader:            newStubReader(),
	}
	_, err := p.EnsureUplinkPort(context.Background())
	if err == nil || !strings.Contains(err.Error(), "br-other") {
		t.Fatalf("expected bridge mismatch error, got: %v", err)
	}
}

func TestPhysical_ExternalCIDR(t *testing.T) {
	rd := newStubReader()
	want := netip.MustParsePrefix("192.0.2.5/24")
	rd.cidrs["br-ext"] = want
	p := &Physical{UplinkBridge: "br-ext", Reader: rd}
	got, err := p.ExternalCIDR(context.Background())
	if err != nil {
		t.Fatalf("ExternalCIDR: %v", err)
	}
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPhysical_ExternalCIDR_NoAddr(t *testing.T) {
	p := &Physical{UplinkBridge: "br-ext", Reader: newStubReader()}
	_, err := p.ExternalCIDR(context.Background())
	if !errors.Is(err, ErrNoUplinkAddr) {
		t.Fatalf("expected ErrNoUplinkAddr, got: %v", err)
	}
}

func TestVeth_EnsureUplinkPort(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl port-to-br veth-wan-ovs", []byte("br-ext\n"), nil)
	rd := newStubReader()
	rd.macs[VethOVSEnd] = mustMAC(t, "02:11:22:33:44:55")
	rd.master[VethLinuxEnd] = "br-wan"

	v := &Veth{
		LinuxBridge:  "br-wan",
		UplinkBridge: "br-ext",
		Runner:       r,
		Reader:       rd,
	}
	mac, err := v.EnsureUplinkPort(context.Background())
	if err != nil {
		t.Fatalf("EnsureUplinkPort: %v", err)
	}
	if mac.String() != "02:11:22:33:44:55" {
		t.Errorf("got MAC %q", mac)
	}
	if v.UplinkMode() != UplinkModeVeth {
		t.Errorf("UplinkMode = %v, want veth", v.UplinkMode())
	}
}

func TestVeth_EnsureUplinkPort_WrongMaster(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl port-to-br veth-wan-ovs", []byte("br-ext\n"), nil)
	rd := newStubReader()
	rd.macs[VethOVSEnd] = mustMAC(t, "02:11:22:33:44:55")
	rd.master[VethLinuxEnd] = "br-other"

	v := &Veth{LinuxBridge: "br-wan", UplinkBridge: "br-ext", Runner: r, Reader: rd}
	_, err := v.EnsureUplinkPort(context.Background())
	if err == nil || !strings.Contains(err.Error(), "br-other") {
		t.Fatalf("expected master mismatch error, got: %v", err)
	}
}

func TestVeth_ExternalCIDR(t *testing.T) {
	rd := newStubReader()
	want := netip.MustParsePrefix("10.0.0.42/24")
	rd.cidrs["br-wan"] = want
	v := &Veth{LinuxBridge: "br-wan", UplinkBridge: "br-ext", Reader: rd}
	got, err := v.ExternalCIDR(context.Background())
	if err != nil {
		t.Fatalf("ExternalCIDR: %v", err)
	}
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestEnsureBridges_RunsExpectedOps(t *testing.T) {
	r := newStubRunner()
	for _, prefix := range []string{
		"ovs-vsctl --may-exist add-br br-int",
		"ovs-vsctl set Bridge br-int fail-mode=secure",
		"ovs-vsctl set Bridge br-int other-config:disable-in-band=true",
		"ip link set br-int up",
		"ovs-vsctl --may-exist add-br br-ext",
		"ip link set br-ext up",
	} {
		r.expect(prefix, nil, nil)
	}
	p := &Physical{ExternalInterface: "enp0s3", UplinkBridge: "br-ext", Runner: r, Reader: newStubReader()}
	if err := p.EnsureBridges(context.Background()); err != nil {
		t.Fatalf("EnsureBridges: %v", err)
	}
	for _, want := range []string{
		"ovs-vsctl --may-exist add-br br-int",
		"ovs-vsctl set Bridge br-int fail-mode=secure",
		"ovs-vsctl --may-exist add-br br-ext",
	} {
		if !r.called(want) {
			t.Errorf("missing call %q; got %v", want, r.calls)
		}
	}
}

func TestEnsureBridges_FailureSurfacesContext(t *testing.T) {
	r := newStubRunner()
	r.expect("ovs-vsctl --may-exist add-br br-int", []byte("permission denied"), errors.New("exit 1"))
	p := &Physical{ExternalInterface: "enp0s3", UplinkBridge: "br-ext", Runner: r}
	err := p.EnsureBridges(context.Background())
	if err == nil || !strings.Contains(err.Error(), "create br-int") {
		t.Fatalf("expected create br-int wrap, got: %v", err)
	}
}

var (
	_ Wiring          = (*Physical)(nil)
	_ Wiring          = (*Veth)(nil)
	_ Runner          = execRunner{}
	_ InterfaceReader = kernelReader{}
)

func TestUplinkModes_ConstantSet(t *testing.T) {
	modes := []UplinkMode{UplinkModeUnknown, UplinkModePhysical, UplinkModeVeth}
	if !slices.Contains(modes, UplinkModePhysical) || !slices.Contains(modes, UplinkModeVeth) {
		t.Fatal("expected physical and veth in supported mode set")
	}
}
