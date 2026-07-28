package main

import (
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"strings"
	"time"
)

// netnsRunDir is the iproute2 netns mount directory; a name here is the netns
// path containerd joins via the OCI network namespace path.
const netnsRunDir = "/var/run/netns"

// Credentials path constants: each task netns gets a /30 veth into the host netns
// from 169.254.172.0/22 and a host-routed /32 to the credential endpoint, keeping
// the endpoint off the task ENI's VPC route. credTaskVeth is the task-side name
// (unique per netns); the host side is per-task (credVethName).
const (
	credTaskVeth     = "ecsc0"
	credEndpointCIDR = defaultCredEndpointIP + "/32"
)

// udhcpcScript is the config script busybox udhcpc execs to apply a lease. It
// is the reason presence of the udhcpc binary is not enough to use it: Debian
// and Ubuntu's busybox ship the applet without this script, and udhcpc then
// takes the lease and exits nonzero having configured nothing.
const udhcpcScript = "/usr/share/udhcpc/default.script"

// netCmdRunner executes a host networking command. Abstracted so unit tests can
// assert the ip/netns sequence without touching the kernel.
type netCmdRunner interface {
	Run(name string, args ...string) (string, error)
}

// execNetRunner runs real commands.
type execNetRunner struct{}

func (execNetRunner) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// taskNetns builds and tears down per-task awsvpc network namespaces. The ENI is
// already hot-plugged into the VM (scheduler did that); the agent finds the NIC
// by MAC, moves it into a task netns, and DHCPs the OVN-assigned ENI IP — the
// same way the primary NIC obtains its address.
type taskNetns struct {
	run     netCmdRunner
	nicWait time.Duration
	poll    time.Duration
	// Injected so the DHCP-client choice below is unit-testable on a host that
	// has neither client installed.
	lookPath   func(string) (string, error)
	fileIsExec func(string) bool
}

func newTaskNetns(run netCmdRunner) *taskNetns {
	return &taskNetns{
		run:        run,
		nicWait:    30 * time.Second,
		poll:       time.Second,
		lookPath:   exec.LookPath,
		fileIsExec: func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.Mode()&0o111 != 0 },
	}
}

// dhcpStep returns the in-netns DHCP command that leases the task ENI's
// OVN-assigned address.
//
// This image ships on two distributions and they do not agree on a DHCP client.
// Alpine has busybox udhcpc; Ubuntu has no udhcpc at all, and `ip netns exec`
// exits 1 — not 127 — when it cannot exec the binary, so a hardcoded udhcpc
// fails as an opaque "network setup failed" with the task stuck in STOPPED.
// Resolve the client at setup time instead, preferring udhcpc so Alpine keeps
// its existing behaviour exactly, then falling back to dhcpcd. Same order and
// same reasoning as dhcp_oneshot() in mulga-mgmt-net.sh.
//
// Both invocations are one-shot: udhcpc -q exits once it has the lease and -n
// gives up rather than backgrounding, and dhcpcd -1 exits after configuring the
// interface. Neither leaves a daemon behind in the netns.
func (n *taskNetns) dhcpStep(name, iface string) ([]string, error) {
	if _, err := n.lookPath("udhcpc"); err == nil && n.fileIsExec(udhcpcScript) {
		return []string{"ip", "netns", "exec", name, "udhcpc", "-i", iface, "-q", "-n"}, nil
	}
	// Lease state is keyed by interface name, which is unique per task here:
	// each task ENI is a separate hot-plug, so it lands on its own PCI slot and
	// gets its own predictable name before being moved into the netns.
	if _, err := n.lookPath("dhcpcd"); err == nil {
		return []string{"ip", "netns", "exec", name, "dhcpcd", "-q", "-1", "-t", "20", iface}, nil
	}
	return nil, fmt.Errorf("no DHCP client for %s: need udhcpc (with %s) or dhcpcd", iface, udhcpcScript)
}

func netnsName(taskID string) string { return "ecs-" + taskID }
func netnsPathFor(taskID string) string {
	return netnsRunDir + "/" + netnsName(taskID)
}

// Setup creates the task netns, moves the ENI NIC (matched by MAC) into it, and
// brings it up with a DHCP lease. It returns the netns path for containerd. On
// any failure it tears the partial netns down so a retry starts clean.
func (n *taskNetns) Setup(taskID, mac string) (string, error) {
	iface, err := n.waitForNIC(mac)
	if err != nil {
		return "", err
	}
	name := netnsName(taskID)
	if _, err := n.run.Run("ip", "netns", "add", name); err != nil {
		return "", fmt.Errorf("netns add %s: %w", name, err)
	}
	hostVeth := credVethName(taskID)
	hostIP, taskIP := credSubnet(taskID)
	dhcp, err := n.dhcpStep(name, iface)
	if err != nil {
		_ = n.Teardown(taskID)
		return "", err
	}
	steps := [][]string{
		{"ip", "link", "set", iface, "netns", name},
		{"ip", "-n", name, "link", "set", "lo", "up"},
		{"ip", "-n", name, "link", "set", iface, "up"},
		dhcp,
		// Credentials path: a veth into the host netns plus a host-routed /32 to the
		// credential endpoint, so the container reaches 169.254.170.2 without it
		// leaking onto the task ENI's VPC route.
		{"ip", "link", "add", hostVeth, "type", "veth", "peer", "name", credTaskVeth, "netns", name},
		{"ip", "addr", "add", hostIP + "/30", "dev", hostVeth},
		{"ip", "link", "set", hostVeth, "up"},
		{"ip", "-n", name, "addr", "add", taskIP + "/30", "dev", credTaskVeth},
		{"ip", "-n", name, "link", "set", credTaskVeth, "up"},
		{"ip", "-n", name, "route", "add", credEndpointCIDR, "via", hostIP},
	}
	for _, s := range steps {
		if _, err := n.run.Run(s[0], s[1:]...); err != nil {
			_ = n.Teardown(taskID)
			return "", fmt.Errorf("netns setup %v: %w", s, err)
		}
	}
	return netnsPathFor(taskID), nil
}

// credVethName is the host-side veth name for a task's credential path. Derived
// from the taskID and kept within the 15-char interface-name limit.
func credVethName(taskID string) string {
	return fmt.Sprintf("ecsv%08x", crc32.ChecksumIEEE([]byte(taskID)))
}

// credSubnet derives a unique /30 in 169.254.172.0/22 for a task's credential
// veth, returning the host-side and task-side addresses. The host side is the
// container's gateway to the credential endpoint.
func credSubnet(taskID string) (hostIP, taskIP string) {
	slot := crc32.ChecksumIEEE([]byte(taskID)) % 256 // 256 /30s in the /22
	third := 172 + (slot*4)/256
	base := (slot * 4) % 256
	hostIP = fmt.Sprintf("169.254.%d.%d", third, base+1)
	taskIP = fmt.Sprintf("169.254.%d.%d", third, base+2)
	return hostIP, taskIP
}

// Teardown deletes the task netns (releasing the moved NIC). Idempotent enough
// for the stop path: a missing netns is reported but not fatal to the caller.
func (n *taskNetns) Teardown(taskID string) error {
	name := netnsName(taskID)
	// Deleting the netns destroys the task-side veth and its host peer; remove the
	// host side explicitly too so a leaked half from a partial setup is reaped.
	_, _ = n.run.Run("ip", "link", "del", credVethName(taskID))
	if _, err := n.run.Run("ip", "netns", "del", name); err != nil {
		return fmt.Errorf("netns del %s: %w", name, err)
	}
	return nil
}

// waitForNIC polls until a NIC with the given MAC appears in the host netns,
// returning its interface name. The hot-plug pipeline is async, so the device
// may lag the assign by a few seconds.
func (n *taskNetns) waitForNIC(mac string) (string, error) {
	want := strings.ToLower(strings.TrimSpace(mac))
	deadline := time.Now().Add(n.nicWait)
	for {
		iface, err := n.findNICByMAC(want)
		if err == nil {
			return iface, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("nic with mac %s not found within %s", want, n.nicWait)
		}
		time.Sleep(n.poll)
	}
}

// findNICByMAC parses `ip -o link` and returns the interface whose link/ether
// matches mac.
func (n *taskNetns) findNICByMAC(mac string) (string, error) {
	out, err := n.run.Run("ip", "-o", "link")
	if err != nil {
		return "", fmt.Errorf("ip -o link: %w", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.Contains(strings.ToLower(line), mac) {
			continue
		}
		// "<idx>: <ifname>: <flags> ... link/ether <mac> ..."
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(strings.TrimSuffix(fields[1], ":"), "@")
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		return name, nil
	}
	return "", fmt.Errorf("no interface with mac %s", mac)
}
