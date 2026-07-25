// Package autoinstall reads headless install parameters from environment
// variables exported by spinifex-init.sh from the kernel cmdline.
// Parameters are set in the GRUB "Headless" menu entry in grub.cfg.
package autoinstall

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/cmd/installer/install"
)

// Load returns an install.Config built from SPINIFEX_* environment variables,
// or nil if SPINIFEX_AUTO is not set to "1" (interactive mode).
func Load() (*install.Config, error) {
	if os.Getenv("SPINIFEX_AUTO") != "1" {
		return nil, nil
	}

	slog.Info("autoinstall: headless mode enabled via kernel cmdline")

	cfg, err := buildConfig()
	if err != nil {
		return nil, fmt.Errorf("autoinstall config: %w", err)
	}
	return cfg, nil
}

// EjectAndReboot ejects the boot device (best-effort) then reboots.
func EjectAndReboot() {
	srcDev, _ := os.ReadFile("/run/iso-dev")
	if dev := strings.TrimSpace(string(srcDev)); dev != "" {
		slog.Info("autoinstall: ejecting boot device", "device", dev)
		_ = exec.Command("eject", dev).Run()
	}

	fmt.Println()
	fmt.Println("Installation complete.")
	fmt.Println("Remove the USB drive now if it was not ejected automatically.")
	fmt.Println("Rebooting in 10 seconds...")
	time.Sleep(10 * time.Second)
	// Use the kernel syscall directly — the live environment runs spinifex-init
	// as PID 1 (not systemd), so exec("reboot") fails trying to reach D-Bus.
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		slog.Error("autoinstall: reboot syscall failed", "err", err)
	}
}

func buildConfig() (*install.Config, error) {
	password := os.Getenv("SPINIFEX_PASSWORD")
	if password == "" {
		return nil, fmt.Errorf("SPINIFEX_PASSWORD is required")
	}

	hostname := os.Getenv("SPINIFEX_HOSTNAME")
	if hostname == "" {
		hostname = "spinifex-node"
	}

	disk, err := resolveDisk(os.Getenv("SPINIFEX_DISK"))
	if err != nil {
		return nil, fmt.Errorf("disk: %w", err)
	}

	wanIface, err := resolveNIC(os.Getenv("SPINIFEX_WAN_IFACE"), "")
	if err != nil {
		return nil, fmt.Errorf("WAN NIC: %w", err)
	}

	cfg := &install.Config{
		Disk:         disk,
		Hostname:     hostname,
		RootPassword: password,
		// Optional on the headless path — empty means no --email passed to
		// spx admin init, which omits the operator identity from telemetry.
		Email: strings.TrimSpace(os.Getenv("SPINIFEX_EMAIL")),
	}

	cfg.WAN, err = parseRole("WAN", wanIface)
	if err != nil {
		return nil, err
	}
	if cfg.WAN.DHCPMode {
		cfg.WAN.Gateway = ""
	} else if cfg.WAN.Gateway == "" {
		return nil, fmt.Errorf("SPINIFEX_WAN_IP, SPINIFEX_WAN_MASK, SPINIFEX_WAN_GW required for static mode")
	}

	// lan and vpc are optional: an unset _IFACE leaves the role folded, which
	// is how a single- or two-NIC node collapses onto the planes above it.
	for _, r := range []struct {
		plane string
		dst   *install.NetworkRole
	}{{"LAN", &cfg.LAN}, {"VPC", &cfg.VPC}} {
		name := os.Getenv("SPINIFEX_" + r.plane + "_IFACE")
		if name == "" {
			continue
		}
		iface, err := resolveNIC(name, wanIface)
		if err != nil {
			return nil, fmt.Errorf("%s NIC: %w", r.plane, err)
		}
		if *r.dst, err = parseRole(r.plane, iface); err != nil {
			return nil, err
		}
		// Only the wan plane installs a default route.
		r.dst.Gateway = ""
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("network roles: %w", err)
	}

	if os.Getenv("SPINIFEX_GPU_PASSTHROUGH") == "1" {
		cfg.GPUPassthrough = true
	}

	role := strings.ToLower(os.Getenv("SPINIFEX_ROLE"))
	if role == "" {
		role = "init"
	}
	cfg.ClusterRole = role
	skipFormation := os.Getenv("SPINIFEX_SKIP_FORMATION") == "1"
	if skipFormation {
		cfg.SkipFormation = true
	}

	if role == "join" && !skipFormation {
		joinAddr := os.Getenv("SPINIFEX_JOIN_ADDR")
		if joinAddr == "" {
			return nil, fmt.Errorf("SPINIFEX_JOIN_ADDR required when SPINIFEX_ROLE=join")
		}
		cfg.JoinAddr = joinAddr
	}

	return cfg, nil
}

// resolveDisk maps the spx_disk value from grub.cfg to a block device path.
//
// Supported values:
//
//	auto            — use the only non-removable disk; fail if multiple found
//	largest         — largest non-removable disk (explicit opt-in)
//	smallest        — smallest non-removable disk (typical OS-on-SSD pattern)
//	nvme            — the only NVMe disk; fail if multiple found
//	/dev/sda (etc.) — exact device path; fail if not present
func resolveDisk(target string) (string, error) {
	switch strings.ToLower(target) {
	case "", "auto":
		return singleDisk()
	case "largest":
		return diskBySize(true)
	case "smallest":
		return diskBySize(false)
	case "nvme":
		return nvmeDisk()
	default:
		if _, err := os.Stat(target); err != nil {
			return "", fmt.Errorf("%q not found", target)
		}
		return target, nil
	}
}

type diskCandidate struct {
	dev   string
	bytes int64
}

// nonRemovableDisks returns all non-removable, non-virtual block devices.
func nonRemovableDisks() ([]diskCandidate, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}
	var out []diskCandidate
	for _, e := range entries {
		dev := e.Name()
		switch {
		case strings.HasPrefix(dev, "loop"),
			strings.HasPrefix(dev, "ram"),
			strings.HasPrefix(dev, "dm-"),
			strings.HasPrefix(dev, "sr"):
			continue
		}
		removable, _ := os.ReadFile("/sys/block/" + dev + "/removable")
		if strings.TrimSpace(string(removable)) == "1" {
			continue
		}
		sizeRaw, _ := os.ReadFile("/sys/block/" + dev + "/size")
		sectors, parseErr := strconv.ParseInt(strings.TrimSpace(string(sizeRaw)), 10, 64)
		if parseErr != nil || sectors == 0 {
			continue
		}
		out = append(out, diskCandidate{dev: dev, bytes: sectors * 512})
	}
	return out, nil
}

// diskList returns a human-readable list of candidates for error messages.
func diskList(disks []diskCandidate) string {
	lines := make([]string, len(disks))
	for i, d := range disks {
		lines[i] = fmt.Sprintf("  /dev/%s (%dG)", d.dev, d.bytes>>30)
	}
	return strings.Join(lines, "\n")
}

// singleDisk selects the only non-removable disk, or fails listing all found.
func singleDisk() (string, error) {
	disks, err := nonRemovableDisks()
	if err != nil {
		return "", err
	}
	switch len(disks) {
	case 0:
		return "", fmt.Errorf("no non-removable disks found")
	case 1:
		slog.Info("autoinstall: single disk selected", "disk", disks[0].dev, "size_gb", disks[0].bytes>>30)
		return "/dev/" + disks[0].dev, nil
	default:
		return "", fmt.Errorf(
			"multiple disks found — set SPINIFEX_DISK to one of:\n%s\n"+
				"  largest   (largest disk)\n"+
				"  smallest  (smallest disk)\n"+
				"  nvme      (NVMe only)\n"+
				"  /dev/sdX  (exact path)",
			diskList(disks),
		)
	}
}

// diskBySize selects the largest (largest=true) or smallest non-removable disk.
func diskBySize(largest bool) (string, error) {
	disks, err := nonRemovableDisks()
	if err != nil {
		return "", err
	}
	if len(disks) == 0 {
		return "", fmt.Errorf("no non-removable disks found")
	}
	sort.Slice(disks, func(i, j int) bool {
		if largest {
			return disks[i].bytes > disks[j].bytes
		}
		return disks[i].bytes < disks[j].bytes
	})
	label := "smallest"
	if largest {
		label = "largest"
	}
	slog.Info("autoinstall: disk selected by size", "mode", label, "disk", disks[0].dev, "size_gb", disks[0].bytes>>30)
	return "/dev/" + disks[0].dev, nil
}

// nvmeDisk selects the only NVMe disk, or fails listing all NVMe disks found.
func nvmeDisk() (string, error) {
	disks, err := nonRemovableDisks()
	if err != nil {
		return "", err
	}
	var nvmes []diskCandidate
	for _, d := range disks {
		if strings.HasPrefix(d.dev, "nvme") {
			nvmes = append(nvmes, d)
		}
	}
	switch len(nvmes) {
	case 0:
		return "", fmt.Errorf("no NVMe disks found")
	case 1:
		slog.Info("autoinstall: NVMe disk selected", "disk", nvmes[0].dev, "size_gb", nvmes[0].bytes>>30)
		return "/dev/" + nvmes[0].dev, nil
	default:
		return "", fmt.Errorf(
			"multiple NVMe disks found — set SPINIFEX_DISK to one of:\n%s",
			diskList(nvmes),
		)
	}
}

// virtualNICPrefixes lists interface name prefixes that identify non-physical
// interfaces (bridges, tunnels, container/OVS veth pairs, etc.). These are
// skipped when auto-selecting a NIC so we don't configure docker0 or
// ovs-system as the management interface on a machine that previously ran
// Docker or OVN.
var virtualNICPrefixes = []string{
	"docker", "veth", "virbr", "br-", "ovs-", "vxlan", "genev", "tun", "tap",
}

// resolveNIC returns the interface name to use. "auto" or empty picks the
// first physical (non-loopback, non-virtual) interface that is not exclude.
// parseRole reads the SPINIFEX_<PLANE>_* kernel-cmdline vars for one plane.
// iface is the already-resolved interface name; _VLAN and _MTU are optional
// and default to untagged at the link MTU.
func parseRole(plane, iface string) (install.NetworkRole, error) {
	role := install.NetworkRole{
		Interface: iface,
		DHCPMode:  strings.ToLower(os.Getenv("SPINIFEX_"+plane+"_MODE")) != "static",
	}
	if v := os.Getenv("SPINIFEX_" + plane + "_VLAN"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil {
			return role, fmt.Errorf("SPINIFEX_%s_VLAN: %w", plane, err)
		}
		role.VLAN = id
	}
	if v := os.Getenv("SPINIFEX_" + plane + "_MTU"); v != "" {
		mtu, err := strconv.Atoi(v)
		if err != nil {
			return role, fmt.Errorf("SPINIFEX_%s_MTU: %w", plane, err)
		}
		role.MTU = mtu
	}
	if !role.DHCPMode {
		role.Address = os.Getenv("SPINIFEX_" + plane + "_IP")
		role.Mask = os.Getenv("SPINIFEX_" + plane + "_MASK")
		role.Gateway = os.Getenv("SPINIFEX_" + plane + "_GW")
		if role.Address == "" || role.Mask == "" {
			return role, fmt.Errorf("SPINIFEX_%s_IP and SPINIFEX_%s_MASK required for static mode", plane, plane)
		}
	}
	if dns := os.Getenv("SPINIFEX_" + plane + "_DNS"); dns != "" {
		role.DNS = strings.Split(dns, ",")
	}
	return role, nil
}

func resolveNIC(name, exclude string) (string, error) {
	if name != "" && name != "auto" {
		return name, nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		// Require broadcast capability — filters out point-to-point tunnels.
		if iface.Flags&net.FlagBroadcast == 0 {
			continue
		}
		// Require a MAC address — virtual/tunnel interfaces have none.
		if len(iface.HardwareAddr) == 0 {
			continue
		}
		if iface.Name == exclude {
			continue
		}
		// Skip known virtual interface prefixes.
		virtual := false
		for _, pfx := range virtualNICPrefixes {
			if strings.HasPrefix(iface.Name, pfx) {
				virtual = true
				break
			}
		}
		if virtual {
			continue
		}
		slog.Info("autoinstall: NIC resolved", "mode", "auto", "selected", iface.Name)
		return iface.Name, nil
	}
	return "", fmt.Errorf("no suitable physical NIC found (non-loopback, broadcast-capable, with MAC)")
}
