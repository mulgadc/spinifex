package host

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedRunner returns canned output keyed by the joined argv, so a test can
// drive different responses for `ip route get`, `ip neigh show` (before/after
// a ping prime), and the ovn-nbctl static-mac-binding calls.
type scriptedRunner struct {
	responses map[string][]byte
	errors    map[string]error
	calls     [][]string
}

func (r *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	full := append([]string{name}, args...)
	r.calls = append(r.calls, full)
	key := strings.Join(full, " ")
	return r.responses[key], r.errors[key]
}

func (r *scriptedRunner) callArgs(prefix string) [][]string {
	var out [][]string
	for _, c := range r.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestSeedNexthopMAC_ResolvedOnFirstRead(t *testing.T) {
	r := &scriptedRunner{
		responses: map[string][]byte{
			"ip route get 192.168.1.1":             []byte("192.168.1.1 dev br-wan src 192.168.1.50"),
			"ip neigh show 192.168.1.1 dev br-wan": []byte("192.168.1.1 dev br-wan lladdr 04:f4:1c:fd:56:27 REACHABLE"),
		},
	}
	if err := SeedNexthopMAC(context.Background(), r, "", "gw-vpc-1", "192.168.1.1"); err != nil {
		t.Fatalf("SeedNexthopMAC: %v", err)
	}
	if calls := r.callArgs("ping"); len(calls) != 0 {
		t.Fatalf("expected no ping prime when neigh resolves first read, got %v", calls)
	}
	delCalls := r.callArgs("ovn-nbctl --if-exists static-mac-binding-del")
	if len(delCalls) != 1 {
		t.Fatalf("expected 1 static-mac-binding-del call, got %d: %v", len(delCalls), r.calls)
	}
	wantDel := "ovn-nbctl --if-exists static-mac-binding-del gw-vpc-1 192.168.1.1"
	if got := strings.Join(delCalls[0], " "); got != wantDel {
		t.Fatalf("del argv mismatch\n got: %s\nwant: %s", got, wantDel)
	}
	addCalls := r.callArgs("ovn-nbctl static-mac-binding-add")
	if len(addCalls) != 1 {
		t.Fatalf("expected 1 static-mac-binding-add call, got %d: %v", len(addCalls), r.calls)
	}
	wantAdd := "ovn-nbctl static-mac-binding-add gw-vpc-1 192.168.1.1 04:f4:1c:fd:56:27"
	if got := strings.Join(addCalls[0], " "); got != wantAdd {
		t.Fatalf("add argv mismatch\n got: %s\nwant: %s", got, wantAdd)
	}
}

// A compute node runs no local database, so the binding has to be addressed to
// the configured NB cluster or the write lands nowhere the fabric reads.
func TestSeedNexthopMAC_WritesToTheConfiguredNBAddress(t *testing.T) {
	r := &scriptedRunner{
		responses: map[string][]byte{
			"ip route get 192.168.1.1":             []byte("192.168.1.1 dev br-wan src 192.168.1.50"),
			"ip neigh show 192.168.1.1 dev br-wan": []byte("192.168.1.1 dev br-wan lladdr 04:f4:1c:fd:56:27 REACHABLE"),
		},
	}
	nb := "tcp:10.2.0.2:6641,tcp:10.2.0.3:6641,tcp:10.2.0.4:6641"
	if err := SeedNexthopMAC(context.Background(), r, nb, "gw-vpc-1", "192.168.1.1"); err != nil {
		t.Fatalf("SeedNexthopMAC: %v", err)
	}

	wantDel := "ovn-nbctl --db=" + nb + " --no-leader-only --if-exists static-mac-binding-del gw-vpc-1 192.168.1.1"
	delCalls := r.callArgs("ovn-nbctl --db=")
	if len(delCalls) != 2 {
		t.Fatalf("expected both nbctl calls to carry --db, got %d: %v", len(delCalls), r.calls)
	}
	if got := strings.Join(delCalls[0], " "); got != wantDel {
		t.Fatalf("del argv mismatch\n got: %s\nwant: %s", got, wantDel)
	}
	wantAdd := "ovn-nbctl --db=" + nb + " --no-leader-only static-mac-binding-add gw-vpc-1 192.168.1.1 04:f4:1c:fd:56:27"
	if got := strings.Join(delCalls[1], " "); got != wantAdd {
		t.Fatalf("add argv mismatch\n got: %s\nwant: %s", got, wantAdd)
	}
}

func TestSeedNexthopMAC_ResolvedAfterPing(t *testing.T) {
	neighKey := "ip neigh show 192.168.1.1 dev br-wan"
	r := &scriptedRunner{
		responses: map[string][]byte{
			"ip route get 192.168.1.1": []byte("192.168.1.1 dev br-wan src 192.168.1.50"),
		},
	}
	// Wrap Run to alternate the neigh response: unresolved, then resolved.
	base := r
	wrapper := &sequencedNeighRunner{
		scriptedRunner: base,
		neighKey:       neighKey,
		firstOut:       []byte(""),
		secondOut:      []byte("192.168.1.1 dev br-wan lladdr 04:f4:1c:fd:56:27 REACHABLE"),
	}
	if err := SeedNexthopMAC(context.Background(), wrapper, "", "gw-vpc-1", "192.168.1.1"); err != nil {
		t.Fatalf("SeedNexthopMAC: %v", err)
	}
	if wrapper.neighCalls != 2 {
		t.Fatalf("expected 2 neigh reads (before/after ping), got %d", wrapper.neighCalls)
	}
	pingCalls := wrapper.callArgs("ping")
	if len(pingCalls) != 1 {
		t.Fatalf("expected exactly 1 ping prime, got %d: %v", len(pingCalls), wrapper.calls)
	}
	wantPing := "ping -c 1 -W 1 192.168.1.1"
	if got := strings.Join(pingCalls[0], " "); got != wantPing {
		t.Fatalf("ping argv mismatch\n got: %s\nwant: %s", got, wantPing)
	}
	addCalls := wrapper.callArgs("ovn-nbctl static-mac-binding-add")
	if len(addCalls) != 1 {
		t.Fatalf("expected seed to proceed after ping resolves the MAC, got %d add calls", len(addCalls))
	}
}

// sequencedNeighRunner returns firstOut on the first `ip neigh show` call for
// neighKey and secondOut thereafter, delegating everything else (including
// call recording) to the embedded scriptedRunner.
type sequencedNeighRunner struct {
	*scriptedRunner

	neighKey   string
	firstOut   []byte
	secondOut  []byte
	neighCalls int
}

func (s *sequencedNeighRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	full := append([]string{name}, args...)
	s.calls = append(s.calls, full)
	if strings.Join(full, " ") == s.neighKey {
		s.neighCalls++
		if s.neighCalls == 1 {
			return s.firstOut, nil
		}
		return s.secondOut, nil
	}
	key := strings.Join(full, " ")
	return s.responses[key], s.errors[key]
}

func TestSeedNexthopMAC_UnresolvedEvenAfterPing(t *testing.T) {
	r := &scriptedRunner{
		responses: map[string][]byte{
			"ip route get 192.168.1.1":             []byte("192.168.1.1 dev br-wan src 192.168.1.50"),
			"ip neigh show 192.168.1.1 dev br-wan": []byte("192.168.1.1 dev br-wan INCOMPLETE"),
		},
	}
	if err := SeedNexthopMAC(context.Background(), r, "", "gw-vpc-1", "192.168.1.1"); err != nil {
		t.Fatalf("SeedNexthopMAC must return nil (best-effort) on unresolved MAC, got: %v", err)
	}
	if calls := r.callArgs("ping"); len(calls) != 1 {
		t.Fatalf("expected exactly 1 ping prime attempt, got %d", len(calls))
	}
	if calls := r.callArgs("ovn-nbctl"); len(calls) != 0 {
		t.Fatalf("expected no ovn-nbctl calls when MAC stays unresolved, got %v", calls)
	}
}

// Routed NAT points the gateway router at 100.127.0.1, an address on this
// host's own transit veth. `ip route get` answers lo and no neigh entry can
// ever exist, so the MAC has to come from the link that owns the address.
func TestSeedNexthopMAC_LocalNexthopResolvesFromOwningLink(t *testing.T) {
	r := &scriptedRunner{
		responses: map[string][]byte{
			"ip route get 100.127.0.1": []byte("local 100.127.0.1 dev lo src 100.127.0.1 uid 0"),
			"ip -4 -o addr show to 100.127.0.1/32": []byte(
				"7: spx-nat-host    inet 100.127.0.1/24 scope global spx-nat-host\\       valid_lft forever preferred_lft forever"),
			"ip -o link show dev spx-nat-host": []byte(
				"7: spx-nat-host@spx-nat-ovs: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP mode DEFAULT group default qlen 1000\\    link/ether 6a:1b:2c:3d:4e:5f brd ff:ff:ff:ff:ff:ff"),
		},
	}
	if err := SeedNexthopMAC(context.Background(), r, "", "gw-vpc-1", "100.127.0.1"); err != nil {
		t.Fatalf("SeedNexthopMAC: %v", err)
	}
	if calls := r.callArgs("ping"); len(calls) != 0 {
		t.Fatalf("expected no ping prime for a host-local nexthop, got %v", calls)
	}
	if calls := r.callArgs("ip neigh"); len(calls) != 0 {
		t.Fatalf("expected no neigh read for a host-local nexthop, got %v", calls)
	}
	addCalls := r.callArgs("ovn-nbctl static-mac-binding-add")
	if len(addCalls) != 1 {
		t.Fatalf("expected 1 static-mac-binding-add call, got %d: %v", len(addCalls), r.calls)
	}
	wantAdd := "ovn-nbctl static-mac-binding-add gw-vpc-1 100.127.0.1 6a:1b:2c:3d:4e:5f"
	if got := strings.Join(addCalls[0], " "); got != wantAdd {
		t.Fatalf("add argv mismatch\n got: %s\nwant: %s", got, wantAdd)
	}
}

func TestSeedNexthopMAC_LocalNexthopNoOwningLink(t *testing.T) {
	r := &scriptedRunner{
		responses: map[string][]byte{
			"ip route get 100.127.0.1":             []byte("local 100.127.0.1 dev lo src 100.127.0.1 uid 0"),
			"ip -4 -o addr show to 100.127.0.1/32": []byte(""),
		},
	}
	if err := SeedNexthopMAC(context.Background(), r, "", "gw-vpc-1", "100.127.0.1"); err != nil {
		t.Fatalf("SeedNexthopMAC must stay best-effort, got: %v", err)
	}
	if calls := r.callArgs("ovn-nbctl"); len(calls) != 0 {
		t.Fatalf("expected no ovn-nbctl calls when no link owns the nexthop, got %v", calls)
	}
}

// A nexthop aliased onto loopback has no ethernet address to bind to.
func TestSeedNexthopMAC_LocalNexthopOnLoopbackOnly(t *testing.T) {
	r := &scriptedRunner{
		responses: map[string][]byte{
			"ip route get 100.127.0.1": []byte("local 100.127.0.1 dev lo src 100.127.0.1 uid 0"),
			"ip -4 -o addr show to 100.127.0.1/32": []byte(
				"1: lo    inet 100.127.0.1/32 scope host lo\\       valid_lft forever preferred_lft forever"),
		},
	}
	if err := SeedNexthopMAC(context.Background(), r, "", "gw-vpc-1", "100.127.0.1"); err != nil {
		t.Fatalf("SeedNexthopMAC must stay best-effort, got: %v", err)
	}
	if calls := r.callArgs("ovn-nbctl"); len(calls) != 0 {
		t.Fatalf("expected no ovn-nbctl calls for a loopback-only nexthop, got %v", calls)
	}
}

func TestParseLinkEtherMAC(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"ether", "7: spx-nat-host@spx-nat-ovs: <UP> mtu 1500\\    link/ether 6a:1b:2c:3d:4e:5f brd ff:ff:ff:ff:ff:ff", "6a:1b:2c:3d:4e:5f"},
		{"loopback", "1: lo: <LOOPBACK,UP> mtu 65536\\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := parseLinkEtherMAC(tc.out); got != tc.want {
			t.Errorf("%s: parseLinkEtherMAC = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSeedNexthopMAC_EmptyArgsNoop(t *testing.T) {
	r := &scriptedRunner{}
	if err := SeedNexthopMAC(context.Background(), r, "", "", "192.168.1.1"); err != nil {
		t.Fatalf("expected nil for empty lrpName, got %v", err)
	}
	if err := SeedNexthopMAC(context.Background(), r, "", "gw-vpc-1", ""); err != nil {
		t.Fatalf("expected nil for empty nexthopIP, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("expected no commands issued for empty args, got %v", r.calls)
	}
}

func TestSeedNexthopMAC_RouteGetFailurePropagates(t *testing.T) {
	r := &scriptedRunner{
		errors: map[string]error{
			"ip route get 192.168.1.1": errors.New("network unreachable"),
		},
	}
	err := SeedNexthopMAC(context.Background(), r, "", "gw-vpc-1", "192.168.1.1")
	if err == nil {
		t.Fatal("expected error when route resolution fails")
	}
}

func TestSeedNexthopMAC_AddFailurePropagates(t *testing.T) {
	r := &scriptedRunner{
		responses: map[string][]byte{
			"ip route get 192.168.1.1":             []byte("192.168.1.1 dev br-wan src 192.168.1.50"),
			"ip neigh show 192.168.1.1 dev br-wan": []byte("192.168.1.1 dev br-wan lladdr 04:f4:1c:fd:56:27 REACHABLE"),
		},
		errors: map[string]error{
			"ovn-nbctl static-mac-binding-add gw-vpc-1 192.168.1.1 04:f4:1c:fd:56:27": errors.New("exit 1"),
		},
	}
	err := SeedNexthopMAC(context.Background(), r, "", "gw-vpc-1", "192.168.1.1")
	if err == nil {
		t.Fatal("expected error when static-mac-binding-add fails")
	}
}
