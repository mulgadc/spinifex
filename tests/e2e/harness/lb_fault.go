//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// qmpSetLinkScript sends set_link to a guest's telemetry QMP socket. The daemon
// holds the main QMP socket for the life of the guest; the telemetry socket is a
// separate QMP server that accepts a second client between collector polls.
const qmpSetLinkScript = `import json, socket, sys
path = "/run/spinifex/qmp-telemetry-%s.sock" % sys.argv[1]
s = socket.socket(socket.AF_UNIX)
s.settimeout(5)
s.connect(path)
f = s.makefile("rw")
f.readline()
def rpc(c):
    f.write(json.dumps(c) + "\n")
    f.flush()
    while True:
        l = json.loads(f.readline())
        if "return" in l or "error" in l:
            return l
rpc({"execute": "qmp_capabilities"})
r = rpc({"execute": "set_link", "arguments": {"name": sys.argv[2], "up": sys.argv[3] == "up"}})
print(json.dumps(r))
sys.exit(0 if "return" in r else 1)
`

// qmpRxFilterScript prints query-rx-filter for every NIC frontend of a guest, so
// a caller can map an ENI's MAC to the device name set_link needs.
const qmpRxFilterScript = `import json, socket, sys
path = "/run/spinifex/qmp-telemetry-%s.sock" % sys.argv[1]
s = socket.socket(socket.AF_UNIX)
s.settimeout(5)
s.connect(path)
f = s.makefile("rw")
f.readline()
def rpc(c):
    f.write(json.dumps(c) + "\n")
    f.flush()
    while True:
        l = json.loads(f.readline())
        if "return" in l or "error" in l:
            return l
rpc({"execute": "qmp_capabilities"})
r = rpc({"execute": "query-rx-filter"})
print(json.dumps(r))
sys.exit(0 if "return" in r else 1)
`

var (
	instanceIDShape = regexp.MustCompile(`^i-[0-9a-f]+$`)
	nicNameShape    = regexp.MustCompile(`^(net|virtio-net-pci\.)[0-9]+$`)
)

// LBSetLink sets one data NIC of a running load balancer guest up or down
// through its telemetry QMP socket, retrying while the collector holds it.
//
// A guest's own interface names are net0..netN in the order the daemon attached
// them; the data NIC is net0 and the management NIC is the last one.
func LBSetLink(t *testing.T, node Node, instanceID, nic string, up bool) error {
	t.Helper()
	if !instanceIDShape.MatchString(instanceID) || !nicNameShape.MatchString(nic) {
		return fmt.Errorf("refusing to inject on %q/%q: not an instance id and a netN or virtio-net-pci.N name", instanceID, nic)
	}
	state := "down"
	if up {
		state = "up"
	}
	cmd := fmt.Sprintf("sudo python3 - %s %s %s <<'PY'\n%sPY", instanceID, nic, state, qmpSetLinkScript)

	var lastErr error
	for attempt := 1; attempt <= 20; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := NewPeerSSH().Run(ctx, node.Addr, cmd)
		cancel()
		if err == nil {
			Detail(t, "qmp_set_link", strings.TrimSpace(string(out)), "node", node.Name, "nic", nic, "state", state)
			return nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("set_link %s %s on %s: %w", nic, state, node.Name, lastErr)
}

// LBNICsByMAC returns a guest's NIC frontend names keyed by lower-case main MAC,
// read from the guest's own QMP query-rx-filter.
func LBNICsByMAC(t *testing.T, node Node, instanceID string) (map[string]string, error) {
	t.Helper()
	if !instanceIDShape.MatchString(instanceID) {
		return nil, fmt.Errorf("%q is not an instance id", instanceID)
	}
	cmd := fmt.Sprintf("sudo python3 - %s <<'PY'\n%sPY", instanceID, qmpRxFilterScript)
	var lastErr error
	for attempt := 1; attempt <= 20; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := NewPeerSSH().Run(ctx, node.Addr, cmd)
		cancel()
		if err == nil {
			var resp struct {
				Return []struct {
					Name    string `json:"name"`
					MainMAC string `json:"main-mac"`
				} `json:"return"`
			}
			if jerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &resp); jerr != nil {
				return nil, fmt.Errorf("parse query-rx-filter: %w: %s", jerr, strings.TrimSpace(string(out)))
			}
			nics := map[string]string{}
			for _, nic := range resp.Return {
				nics[strings.ToLower(nic.MainMAC)] = nic.Name
			}
			return nics, nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("query-rx-filter on %s: %w", node.Name, lastErr)
}

// ProcessIdentity returns the pid and start time of the QEMU process running an
// instance on node. A reboot, a relaunch or a restart changes it, so equality
// before and after is evidence the guest was left alone, and inequality is
// evidence it was not.
func ProcessIdentity(node Node, instanceID string) (string, error) {
	if !instanceIDShape.MatchString(instanceID) {
		return "", fmt.Errorf("%q is not an instance id", instanceID)
	}
	cmd := fmt.Sprintf(
		"ps -eo pid=,lstart=,args= | grep -E '[q]emu.*%s' | head -1 | awk '{print $1, $2, $3, $4, $5, $6}'",
		instanceID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := NewPeerSSH().Run(ctx, node.Addr, cmd)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("no qemu process for %s on %s", instanceID, node.Name)
	}
	return id, nil
}

// DaemonLogLines returns the spinifex-daemon journal lines on every node that
// contain all of the given needles, since the given time.
func DaemonLogLines(c *Cluster, since time.Time, needles ...string) map[string][]string {
	grep := ""
	for _, n := range needles {
		grep += fmt.Sprintf(" | grep -F %s", ShellQuote(n))
	}
	out := map[string][]string{}
	for _, node := range c.Nodes {
		cmd := fmt.Sprintf("sudo journalctl -u spinifex-daemon --no-pager --since @%d%s || true", since.Unix(), grep)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		raw, err := NewPeerSSH().Run(ctx, node.Addr, cmd)
		cancel()
		if err != nil {
			out[node.Name] = []string{"(unreadable: " + err.Error() + ")"}
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.TrimSpace(line) != "" {
				out[node.Name] = append(out[node.Name], line)
			}
		}
	}
	return out
}
