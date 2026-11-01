// Package nodefacts describes a node's hardware, OS and cabling for the
// operator console. The root-only probes run once at daemon start through
// Collect; Build reads their output and adds what sysfs reports live.
package nodefacts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultDir is where Collect writes and Build reads.
const DefaultDir = "/etc/spinifex/node-facts"

const probeTimeout = 10 * time.Second

// Raw probe files under the facts directory.
const (
	fileIdentity  = "dmi-identity.txt"
	fileProcessor = "dmi-processor.txt"
	fileMemory    = "dmi-memory.txt"
	fileVirt      = "virt.txt"
	fileLLDP      = "lldp.json"
	fileEncapIP   = "ovn-encap-ip.txt"
	fileCollected = "collected-at.txt"
	nicPrefix     = "nic-"
)

// Tools are looked up by absolute path: /usr/sbin is not on a service's PATH.
var (
	dmidecodePaths  = []string{"/usr/sbin/dmidecode", "/sbin/dmidecode"}
	ethtoolPaths    = []string{"/usr/sbin/ethtool", "/sbin/ethtool"}
	networkctlPaths = []string{"/usr/bin/networkctl", "/bin/networkctl"}
	ovsVsctlPaths   = []string{"/usr/bin/ovs-vsctl", "/usr/sbin/ovs-vsctl"}
	udevadmPaths    = []string{"/usr/bin/udevadm", "/bin/udevadm"}
	detectVirtPaths = []string{"/usr/bin/systemd-detect-virt", "/bin/systemd-detect-virt"}
)

func findTool(paths []string) string {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// Collect runs the probes that need root and writes their raw output under
// dir, readable by the spinifex group. A probe that fails leaves no file, so
// Build reports that source as unavailable rather than stopping the daemon.
func Collect(ctx context.Context, dir, sysRoot string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	gid := spinifexGID()
	if gid >= 0 {
		_ = os.Chown(dir, 0, gid)
	}
	write := func(name string, data []byte) {
		if err := writeFile(dir, name, data, gid); err != nil {
			slog.Warn("node-facts: write failed", "file", name, "err", err)
		}
	}

	if dmi := findTool(dmidecodePaths); dmi != "" {
		for name, args := range map[string][]string{
			fileIdentity:  {"-t", "bios", "-t", "system", "-t", "chassis", "-t", "baseboard"},
			fileProcessor: {"-t", "processor"},
			fileMemory:    {"-t", "memory"},
		} {
			if out, err := run(ctx, dmi, args...); err == nil {
				write(name, out)
			} else {
				slog.Info("node-facts: dmidecode unavailable", "type", name, "err", err)
			}
		}
	}

	if virt := findTool(detectVirtPaths); virt != "" {
		// Exits 1 on bare metal while printing "none", so the output counts.
		out, _ := run(ctx, virt)
		write(fileVirt, out)
	}

	if ctl := findTool(networkctlPaths); ctl != "" {
		if out, err := run(ctx, ctl, "lldp", "--json=short"); err == nil {
			write(fileLLDP, out)
		}
	}

	if vsctl := findTool(ovsVsctlPaths); vsctl != "" {
		if out, err := run(ctx, vsctl, "get", "Open_vSwitch", ".", "external_ids:ovn-encap-ip"); err == nil {
			write(fileEncapIP, []byte(strings.Trim(strings.TrimSpace(string(out)), `"`)))
		}
	}

	ethtool, udevadm, vsctl := findTool(ethtoolPaths), findTool(udevadmPaths), findTool(ovsVsctlPaths)
	for _, nic := range physicalNICs(sysRoot) {
		var b strings.Builder
		if ethtool != "" {
			out, _ := run(ctx, ethtool, nic)
			b.Write(out)
			out, _ = run(ctx, ethtool, "-i", nic)
			b.Write(out)
		}
		if udevadm != "" {
			out, _ := run(ctx, udevadm, "info", "--query=property", "--path=/sys/class/net/"+nic)
			b.Write(out)
		}
		if vsctl != "" {
			if out, err := run(ctx, vsctl, "iface-to-br", nic); err == nil {
				fmt.Fprintf(&b, "OVS_BRIDGE=%s\n", strings.TrimSpace(string(out)))
			}
		}
		write(nicPrefix+nic+".txt", []byte(b.String()))
	}

	write(fileCollected, []byte(time.Now().UTC().Format(time.RFC3339)))
	return nil
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return out, nil
}

// writeFile replaces dir/name atomically. The files carry serial numbers, so
// they are root-owned and readable only by the spinifex group.
func writeFile(dir, name string, data []byte, gid int) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return err
	}
	if gid >= 0 {
		_ = tmp.Chown(0, gid)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

func spinifexGID() int {
	g, err := user.LookupGroup("spinifex")
	if err != nil {
		return -1
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return -1
	}
	return gid
}

// physicalNICs lists interfaces backed by a device, which leaves out bridges,
// taps, veths and VLANs.
func physicalNICs(sysRoot string) []string {
	entries, err := os.ReadDir(filepath.Join(sysRoot, "class", "net"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		base := filepath.Join(sysRoot, "class", "net", e.Name())
		if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
			continue
		}
		// A VF or an SR-IOV representor still has a device; its phys_port_name
		// marks it, and it is not cabled to anything.
		if name, err := os.ReadFile(filepath.Join(base, "phys_port_name")); err == nil && strings.HasPrefix(strings.TrimSpace(string(name)), "pf") {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

var errNoFile = errors.New("not collected")

func readFacts(dir, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoFile
	}
	return data, err
}
