package host

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const vethClamp1408 = `add table inet spinifex_mss
delete table inet spinifex_mss
add table bridge spinifex_mss
delete table bridge spinifex_mss
table bridge spinifex_mss {
	chain clamp {
		type filter hook forward priority -150; policy accept;
		iifname "veth-wan-br" ether type ip tcp flags & (syn | rst) == syn tcp option maxseg size > 1368 tcp option maxseg size set 1368
		oifname "veth-wan-br" ether type ip tcp flags & (syn | rst) == syn tcp option maxseg size > 1368 tcp option maxseg size set 1368
		iifname "veth-wan-br" ether type ip6 tcp flags & (syn | rst) == syn tcp option maxseg size > 1348 tcp option maxseg size set 1348
		oifname "veth-wan-br" ether type ip6 tcp flags & (syn | rst) == syn tcp option maxseg size > 1348 tcp option maxseg size set 1348
	}
}
`

const routedClamp1442 = `add table bridge spinifex_mss
delete table bridge spinifex_mss
add table inet spinifex_mss
delete table inet spinifex_mss
table inet spinifex_mss {
	chain clamp {
		type filter hook forward priority -150; policy accept;
		iifname "spx-nat-host" meta nfproto ipv4 tcp flags & (syn | rst) == syn tcp option maxseg size > 1402 tcp option maxseg size set 1402
		oifname "spx-nat-host" meta nfproto ipv4 tcp flags & (syn | rst) == syn tcp option maxseg size > 1402 tcp option maxseg size set 1402
		iifname "spx-nat-host" meta nfproto ipv6 tcp flags & (syn | rst) == syn tcp option maxseg size > 1382 tcp option maxseg size set 1382
		oifname "spx-nat-host" meta nfproto ipv6 tcp flags & (syn | rst) == syn tcp option maxseg size > 1382 tcp option maxseg size set 1382
	}
}
`

func TestMSSClampRuleset(t *testing.T) {
	tests := []struct {
		name string
		mode UplinkMode
		mtu  int
		want string
	}{
		{"veth ipsec on", UplinkModeVeth, 1408, vethClamp1408},
		{"routed ipsec off", UplinkModeRouted, 1442, routedClamp1442},
		{"physical has no host hop", UplinkModePhysical, 1408, ""},
		{"unknown mode", UplinkModeUnknown, 1408, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mssClampRuleset(tt.mode, tt.mtu); got != tt.want {
				t.Errorf("ruleset mismatch\ngot:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestMSSClampRuleset_FollowsGatewayMTU(t *testing.T) {
	got := mssClampRuleset(UplinkModeVeth, 1442)
	for _, want := range []string{"size set 1402", "size set 1382"} {
		if !strings.Contains(got, want) {
			t.Errorf("1442 ruleset missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "1368") || strings.Contains(got, "1348") {
		t.Errorf("1442 ruleset carries the 1408 values:\n%s", got)
	}
}

func TestEnsureMSSClamp_AppliesOneTransaction(t *testing.T) {
	r := newStubRunner()
	r.expect("nft ", nil, nil)
	if err := EnsureMSSClamp(context.Background(), r, UplinkModeVeth, 1408); err != nil {
		t.Fatalf("EnsureMSSClamp: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0] != "nft "+vethClamp1408 {
		t.Errorf("want exactly one nft call carrying the full batch, got %q", r.calls)
	}
}

func TestEnsureMSSClamp_NoOpCases(t *testing.T) {
	tests := map[string]struct {
		mode UplinkMode
		mtu  int
	}{
		"unknown mode": {UplinkModeUnknown, 1408},
		"zero gateway": {UplinkModeVeth, 0},
		"negative":     {UplinkModeRouted, -1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := newStubRunner()
			if err := EnsureMSSClamp(context.Background(), r, tc.mode, tc.mtu); err != nil {
				t.Fatalf("EnsureMSSClamp: %v", err)
			}
			if len(r.calls) != 0 {
				t.Errorf("expected no nft call, got %q", r.calls)
			}
		})
	}
}

func TestEnsureMSSClamp_PhysicalClearsStaleTables(t *testing.T) {
	r := newStubRunner()
	r.expect("nft ", nil, nil)
	if err := EnsureMSSClamp(context.Background(), r, UplinkModePhysical, 1408); err != nil {
		t.Fatalf("EnsureMSSClamp: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0] != "nft "+mssClampTeardown {
		t.Errorf("want one teardown call, got %q", r.calls)
	}
	if strings.Contains(r.calls[0], "chain clamp") {
		t.Errorf("physical mode must not define a table: %s", r.calls[0])
	}
}

func TestEnsureMSSClamp_PhysicalToleratesTeardownFailure(t *testing.T) {
	r := newStubRunner()
	r.expect("nft ", nil, errors.New("exec: nft not found"))
	if err := EnsureMSSClamp(context.Background(), r, UplinkModePhysical, 1408); err != nil {
		t.Errorf("a failed best-effort cleanup must not fail physical mode: %v", err)
	}
}

func TestEnsureMSSClamp_WrapsRunnerErrorWithMode(t *testing.T) {
	r := newStubRunner()
	r.expect("nft ", []byte("Error: Operation not permitted"), errors.New("exit status 1"))
	err := EnsureMSSClamp(context.Background(), r, UplinkModeRouted, 1408)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"routed mode", "Operation not permitted", "exit status 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestRemoveMSSClamp(t *testing.T) {
	r := newStubRunner()
	r.expect("nft ", nil, nil)
	if err := RemoveMSSClamp(context.Background(), r); err != nil {
		t.Fatalf("RemoveMSSClamp: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0] != "nft "+mssClampTeardown {
		t.Errorf("want one teardown call, got %q", r.calls)
	}

	r = newStubRunner()
	r.expect("nft ", []byte("denied"), errors.New("exit status 1"))
	if err := RemoveMSSClamp(context.Background(), r); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("runner failure must be returned with output, got %v", err)
	}
}
