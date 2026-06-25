package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNetRunner records every command and serves canned `ip -o link` output. A
// failOn substring forces the matching command to error so teardown paths run.
type fakeNetRunner struct {
	mu      sync.Mutex
	calls   [][]string
	linkOut string
	failOn  string
}

func (f *fakeNetRunner) Run(name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := append([]string{name}, args...)
	f.calls = append(f.calls, cmd)
	joined := strings.Join(cmd, " ")
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		return "", errors.New("boom: " + joined)
	}
	if name == "ip" && len(args) >= 2 && args[0] == "-o" && args[1] == "link" {
		return f.linkOut, nil
	}
	return "", nil
}

func (f *fakeNetRunner) joined() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func (f *fakeNetRunner) sawAny(sub string) bool {
	for _, j := range f.joined() {
		if strings.Contains(j, sub) {
			return true
		}
	}
	return false
}

const linkWithENI = `1: lo: <LOOPBACK,UP> mtu 65536 ... link/loopback 00:00:00:00:00:00 ...
2: eth0: <BROADCAST,UP> mtu 1500 ... link/ether 52:54:00:aa:bb:cc ...
3: eth1: <BROADCAST,UP> mtu 1500 ... link/ether 52:54:00:de:ad:01 ...`

func newTestNetns(f *fakeNetRunner) *taskNetns {
	n := newTaskNetns(f)
	n.nicWait = 50 * time.Millisecond
	n.poll = 5 * time.Millisecond
	return n
}

func TestNetns_FindNICByMAC(t *testing.T) {
	n := newTestNetns(&fakeNetRunner{linkOut: linkWithENI})
	iface, err := n.findNICByMAC("52:54:00:de:ad:01")
	if err != nil || iface != "eth1" {
		t.Fatalf("want eth1, got %q err=%v", iface, err)
	}
	if _, err := n.findNICByMAC("52:54:00:ff:ff:ff"); err == nil {
		t.Fatal("want error for absent mac")
	}
}

func TestNetns_SetupSequence(t *testing.T) {
	f := &fakeNetRunner{linkOut: linkWithENI}
	n := newTestNetns(f)
	path, err := n.Setup("t-001", "52:54:00:DE:AD:01")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if path != netnsPathFor("t-001") {
		t.Fatalf("want %q, got %q", netnsPathFor("t-001"), path)
	}
	hostIP, taskIP := credSubnet("t-001")
	hostVeth := credVethName("t-001")
	want := []string{
		"ip netns add ecs-t-001",
		"ip link set eth1 netns ecs-t-001",
		"ip -n ecs-t-001 link set lo up",
		"ip -n ecs-t-001 link set eth1 up",
		"ip netns exec ecs-t-001 udhcpc -i eth1 -q -n",
		"ip link add " + hostVeth + " type veth peer name ecsc0 netns ecs-t-001",
		"ip addr add " + hostIP + "/30 dev " + hostVeth,
		"ip -n ecs-t-001 addr add " + taskIP + "/30 dev ecsc0",
		"ip -n ecs-t-001 route add 169.254.170.2/32 via " + hostIP,
	}
	for _, w := range want {
		if !f.sawAny(w) {
			t.Errorf("missing command %q in %v", w, f.joined())
		}
	}
}

func TestCredSubnet_UniquePer30(t *testing.T) {
	hostA, taskA := credSubnet("task-a")
	hostB, taskB := credSubnet("task-b")
	if hostA == hostB {
		t.Errorf("distinct tasks share a host IP: %s", hostA)
	}
	for _, ip := range []string{hostA, taskA, hostB, taskB} {
		if !strings.HasPrefix(ip, "169.254.17") {
			t.Errorf("cred IP %s outside 169.254.172.0/22", ip)
		}
	}
	// host and task ends of one task sit in the same /30 (differ in last octet).
	if hostA[:strings.LastIndexByte(hostA, '.')] != taskA[:strings.LastIndexByte(taskA, '.')] {
		t.Errorf("host %s and task %s not in same /30", hostA, taskA)
	}
}

func TestNetns_SetupNICNotFound(t *testing.T) {
	f := &fakeNetRunner{linkOut: linkWithENI}
	n := newTestNetns(f)
	if _, err := n.Setup("t-001", "52:54:00:00:00:99"); err == nil {
		t.Fatal("want timeout error for absent NIC")
	}
	if f.sawAny("netns add") {
		t.Error("should not create netns when NIC never appears")
	}
}

func TestNetns_SetupTearsDownOnStepFailure(t *testing.T) {
	f := &fakeNetRunner{linkOut: linkWithENI, failOn: "udhcpc"}
	n := newTestNetns(f)
	if _, err := n.Setup("t-001", "52:54:00:de:ad:01"); err == nil {
		t.Fatal("want error when udhcpc fails")
	}
	if !f.sawAny("ip netns del ecs-t-001") {
		t.Errorf("expected teardown after step failure, got %v", f.joined())
	}
}

func TestNetns_Teardown(t *testing.T) {
	f := &fakeNetRunner{}
	n := newTestNetns(f)
	if err := n.Teardown("t-001"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !f.sawAny("ip netns del ecs-t-001") {
		t.Errorf("expected netns del, got %v", f.joined())
	}
}
