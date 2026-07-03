package host

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const (
	wiredRouteGet = "192.168.1.1 dev eth0 src 192.168.1.42 uid 0\n    cache\n"
	wifiRouteGet  = "192.168.1.1 dev wlan0 proto dhcp src 192.168.1.87 metric 600\n    cache\n"
)

func TestDetectUplinkFor(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		wantIface string
		wantSrc   string
		wantErr   bool
	}{
		{name: "wired", out: wiredRouteGet, wantIface: "eth0", wantSrc: "192.168.1.42"},
		{name: "wifi with metric", out: wifiRouteGet, wantIface: "wlan0", wantSrc: "192.168.1.87"},
		{name: "no dev", out: "unreachable 192.168.1.1\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newStubRunner()
			r.expect("ip route get", []byte(tt.out), nil)
			iface, src, err := DetectUplinkFor(context.Background(), r, "192.168.1.1")
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("DetectUplinkFor: %v", err)
			}
			if iface != tt.wantIface || src != tt.wantSrc {
				t.Errorf("got (%q, %q), want (%q, %q)", iface, src, tt.wantIface, tt.wantSrc)
			}
		})
	}
}

func TestDetectUplinkFor_LookupFailure(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route get", []byte("RTNETLINK answers: Network is unreachable"), fmt.Errorf("exit 2"))
	if _, _, err := DetectUplinkFor(context.Background(), r, "192.168.1.1"); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("expected lookup error with output, got: %v", err)
	}
}

func newEIPStubRunner(routeGet string) *stubRunner {
	r := newStubRunner()
	r.expect("ip route get", []byte(routeGet), nil)
	r.expect("ip route replace", nil, nil)
	r.expect("ip neigh replace proxy", nil, nil)
	r.expect("sysctl -w", nil, nil)
	r.expect("arping", nil, nil)
	r.expect("iptables -t filter -C", []byte("iptables: No chain/target/match by that name."), fmt.Errorf("exit 1"))
	r.expect("iptables -t filter -A", nil, nil)
	return r
}

func TestEnsureEIPIngress_FullPlumbing(t *testing.T) {
	r := newEIPStubRunner(wiredRouteGet)
	if err := EnsureEIPIngress(context.Background(), r, "192.168.1.200", "100.127.0.10", "192.168.1.1"); err != nil {
		t.Fatalf("EnsureEIPIngress: %v", err)
	}
	for _, want := range []string{
		"ip route replace 192.168.1.200/32 via 100.127.0.10 dev " + NATTransitHostEnd + " src 192.168.1.42",
		"ip neigh replace proxy 192.168.1.200 dev eth0",
		"sysctl -w net.ipv4.neigh.eth0.proxy_delay=0",
		"arping -U -c 2 -I eth0 192.168.1.200",
		"iptables -t filter -A FORWARD -i " + NATTransitHostEnd + " -s 192.168.1.200/32",
		"iptables -t filter -A FORWARD -o " + NATTransitHostEnd + " -d 192.168.1.200/32",
	} {
		if !r.called(want) {
			t.Errorf("missing call:\n  want %q\n  got  %v", want, r.calls)
		}
	}
}

func TestEnsureEIPIngress_WiFiUplink(t *testing.T) {
	r := newEIPStubRunner(wifiRouteGet)
	if err := EnsureEIPIngress(context.Background(), r, "192.168.1.200", "100.127.0.10", "192.168.1.1"); err != nil {
		t.Fatalf("EnsureEIPIngress: %v", err)
	}
	if !r.called("ip neigh replace proxy 192.168.1.200 dev wlan0") {
		t.Errorf("proxy-ARP must land on detected wifi uplink: %v", r.calls)
	}
	if !r.called("ip route replace 192.168.1.200/32 via 100.127.0.10 dev " + NATTransitHostEnd + " src 192.168.1.87") {
		t.Errorf("route must carry wifi src IP: %v", r.calls)
	}
}

func TestEnsureEIPIngress_IdempotentSkipsExistingRules(t *testing.T) {
	r := newEIPStubRunner(wiredRouteGet)
	r.expect("iptables -t filter -C", nil, nil)
	if err := EnsureEIPIngress(context.Background(), r, "192.168.1.200", "100.127.0.10", "192.168.1.1"); err != nil {
		t.Fatalf("EnsureEIPIngress: %v", err)
	}
	if r.called("iptables -t filter -A") {
		t.Errorf("existing FORWARD rules must not be re-appended: %v", r.calls)
	}
}

func TestEnsureEIPIngress_ArpingFailureNonFatal(t *testing.T) {
	r := newEIPStubRunner(wiredRouteGet)
	r.expect("arping", []byte("arping: command not found"), fmt.Errorf("exit 127"))
	r.expect("sysctl -w", []byte("permission denied"), fmt.Errorf("exit 1"))
	if err := EnsureEIPIngress(context.Background(), r, "192.168.1.200", "100.127.0.10", "192.168.1.1"); err != nil {
		t.Fatalf("arping/sysctl failures must be non-fatal: %v", err)
	}
}

func TestEnsureEIPIngress_NoGatewaySkipsUplinkPlumbing(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route replace", nil, nil)
	r.expect("iptables -t filter -C", nil, nil)
	if err := EnsureEIPIngress(context.Background(), r, "192.168.1.200", "100.127.0.10", ""); err != nil {
		t.Fatalf("EnsureEIPIngress: %v", err)
	}
	if !r.called("ip route replace 192.168.1.200/32 via 100.127.0.10 dev " + NATTransitHostEnd) {
		t.Errorf("route must install without gateway: %v", r.calls)
	}
	if r.called("ip neigh") || r.called("ip route get") {
		t.Errorf("no proxy-ARP or uplink detection without gateway: %v", r.calls)
	}
	if strings.Contains(strings.Join(r.calls, " "), " src ") {
		t.Errorf("no src hint without uplink detection: %v", r.calls)
	}
}

func TestEnsureEIPIngress_ValidatesArgs(t *testing.T) {
	r := newStubRunner()
	if err := EnsureEIPIngress(context.Background(), r, "", "100.127.0.10", ""); err == nil {
		t.Error("empty eip: expected error")
	}
	if err := EnsureEIPIngress(context.Background(), r, "192.168.1.200", "", ""); err == nil {
		t.Error("empty gwLrpIP: expected error")
	}
	if len(r.calls) != 0 {
		t.Errorf("validation failures must not run commands: %v", r.calls)
	}
}

func TestRemoveEIPIngress_TearsDownAll(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route get", []byte(wiredRouteGet), nil)
	r.expect("ip route del", nil, nil)
	r.expect("ip neigh del proxy", nil, nil)
	r.expect("iptables -t filter -D", nil, nil)
	if err := RemoveEIPIngress(context.Background(), r, "192.168.1.200", "192.168.1.1"); err != nil {
		t.Fatalf("RemoveEIPIngress: %v", err)
	}
	for _, want := range []string{
		"ip route del 192.168.1.200/32 dev " + NATTransitHostEnd,
		"ip neigh del proxy 192.168.1.200 dev eth0",
		"iptables -t filter -D FORWARD -i " + NATTransitHostEnd + " -s 192.168.1.200/32",
		"iptables -t filter -D FORWARD -o " + NATTransitHostEnd + " -d 192.168.1.200/32",
	} {
		if !r.called(want) {
			t.Errorf("missing teardown call:\n  want %q\n  got  %v", want, r.calls)
		}
	}
}

func TestRemoveEIPIngress_MissingStateNotError(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route get", []byte("RTNETLINK answers: Network is unreachable"), fmt.Errorf("exit 2"))
	r.expect("ip route del", []byte("RTNETLINK answers: No such process"), fmt.Errorf("exit 2"))
	r.expect("iptables -t filter -D", []byte("Bad rule"), fmt.Errorf("exit 1"))
	if err := RemoveEIPIngress(context.Background(), r, "192.168.1.200", "192.168.1.1"); err != nil {
		t.Fatalf("missing state must not be an error, got: %v", err)
	}
}
