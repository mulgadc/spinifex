// Package autoinstall reads headless install parameters from environment
// variables exported by spinifex-init.sh from the kernel cmdline.
// Parameters are set in the GRUB "Headless" menu entry in grub.cfg.
package autoinstall

import (
	"cmp"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/cmd/installer/install"
)

// listDisks is the block-device scan, indirected so tests can drive selection
// without real hardware.
var listDisks = install.ListDisks

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

	storage, err := resolveStorage()
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}

	wanIface, err := resolveNIC(os.Getenv("SPINIFEX_WAN_IFACE"), "")
	if err != nil {
		return nil, fmt.Errorf("WAN NIC: %w", err)
	}

	cfg := &install.Config{
		Storage:      storage,
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

	if os.Getenv("SPINIFEX_SKIP_FORMATION") == "1" {
		cfg.SkipFormation = true
	}

	return cfg, nil
}

// resolveStorage builds the disk configuration from the kernel cmdline.
//
//	SPINIFEX_FS               ext4 (default) or one of the zfs-* topologies
//	SPINIFEX_DISKS            explicit ordered member list, required for multi-disk pools
//	SPINIFEX_DISK             the OS disk, as before
//	SPINIFEX_DISK_SPINIFEX    ext4 only: dedicated disk for /var/lib/spinifex
//	SPINIFEX_DISK_PREDASTORE  ext4 only: dedicated disk for /var/lib/spinifex/predastore
func resolveStorage() (install.DiskConfig, error) {
	cfg := install.DiskConfig{FS: install.FSExt4}
	if v := strings.TrimSpace(os.Getenv("SPINIFEX_FS")); v != "" {
		fs, err := install.ParseFSType(v)
		if err != nil {
			return cfg, err
		}
		cfg.FS = fs
	}

	var err error
	switch list := strings.TrimSpace(os.Getenv("SPINIFEX_DISKS")); {
	case list != "":
		if cfg.Disks, err = disksByName(strings.Split(list, ",")); err != nil {
			return cfg, err
		}
	case cfg.FS.MinDisks() > 1:
		// No auto-selection for a multi-disk pool. Choosing which disks to
		// erase is not a decision an unattended install gets to make, and the
		// member order determines the RAID10 pairing.
		return cfg, fmt.Errorf("%s needs at least %d disks — set SPINIFEX_DISKS to an explicit comma-separated list",
			cfg.FS.Label(), cfg.FS.MinDisks())
	default:
		d, derr := resolveDisk(os.Getenv("SPINIFEX_DISK"))
		if derr != nil {
			return cfg, derr
		}
		cfg.Disks = []install.Disk{d}
	}

	if err := applyDiskRoles(&cfg); err != nil {
		return cfg, err
	}
	if cfg.ZFS, err = parseZFSOpts(); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	slog.Info("autoinstall: storage selected", "fs", cfg.FS, "disks", cfg.Paths())
	return cfg, nil
}

// roleVars maps each optional data role to the variable that assigns it. The OS
// role is not here: it always comes from SPINIFEX_DISK.
var roleVars = []struct {
	role install.DiskRole
	env  string
}{
	{install.RoleSpinifex, "SPINIFEX_DISK_SPINIFEX"},
	{install.RolePredastore, "SPINIFEX_DISK_PREDASTORE"},
}

// applyDiskRoles attaches dedicated data drives to an ext4 install. Roles are
// always set on ext4, even with no extra drives, so the OS assignment is
// explicit rather than implied by position.
func applyDiskRoles(cfg *install.DiskConfig) error {
	if cfg.FS.IsZFS() {
		for _, rv := range roleVars {
			if strings.TrimSpace(os.Getenv(rv.env)) != "" {
				return fmt.Errorf("%s applies to ext4 only — %s spans every member already", rv.env, cfg.FS.Label())
			}
		}
		return nil
	}
	if len(cfg.Disks) != 1 {
		return fmt.Errorf("ext4 takes one OS disk, %d given — set SPINIFEX_DISK, and SPINIFEX_DISK_SPINIFEX or SPINIFEX_DISK_PREDASTORE for dedicated drives",
			len(cfg.Disks))
	}

	roles := []install.RoleMount{{Role: install.RoleOS, Disk: cfg.Disks[0]}}
	for _, rv := range roleVars {
		spec := strings.TrimSpace(os.Getenv(rv.env))
		if spec == "" {
			continue
		}
		// Resolved with the same fail-closed selector as the OS disk, so an
		// ambiguous value lists the candidates rather than picking one.
		d, err := resolveDisk(spec)
		if err != nil {
			return fmt.Errorf("%s: %w", rv.env, err)
		}
		roles = append(roles, install.RoleMount{Role: rv.role, Disk: d})
	}
	*cfg = cfg.WithRoles(roles)
	return nil
}

// parseZFSOpts reads the advanced pool tunables. Every one is optional; unset
// values are computed from the hardware at install time.
func parseZFSOpts() (install.ZFSOpts, error) {
	var o install.ZFSOpts
	ints := []struct {
		env string
		dst *int
	}{
		{"SPINIFEX_ZFS_ASHIFT", &o.Ashift},
		{"SPINIFEX_ZFS_COPIES", &o.Copies},
		{"SPINIFEX_ZFS_ARC_MAX_MIB", &o.ARCMaxMiB},
	}
	for _, f := range ints {
		v := strings.TrimSpace(os.Getenv(f.env))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return o, fmt.Errorf("%s: %w", f.env, err)
		}
		*f.dst = n
	}
	o.Compress = strings.TrimSpace(os.Getenv("SPINIFEX_ZFS_COMPRESS"))
	o.Checksum = strings.TrimSpace(os.Getenv("SPINIFEX_ZFS_CHECKSUM"))
	return o, nil
}

// disksByName resolves an explicit member list. Names may be kernel names
// ("sdb"), device paths or by-id paths; the order given is preserved because
// RAID10 pairs members in it.
func disksByName(names []string) ([]install.Disk, error) {
	available, err := listDisks()
	if err != nil {
		return nil, err
	}
	out := make([]install.Disk, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		// A by-id path or a symlink resolves to the kernel device the scan
		// reported, so both spellings name the same disk.
		resolved := name
		if !strings.HasPrefix(resolved, "/dev/") {
			resolved = "/dev/" + resolved
		}
		if target, err := filepath.EvalSymlinks(resolved); err == nil {
			resolved = target
		}

		idx := slices.IndexFunc(available, func(d install.Disk) bool { return d.Path == resolved })
		if idx < 0 {
			return nil, fmt.Errorf("%q is not an available disk — found:\n%s", name, diskList(available))
		}
		out = append(out, available[idx])
	}
	return out, nil
}

// resolveDisk maps the SPINIFEX_DISK value from grub.cfg to a disk.
//
// Supported values:
//
//	auto            — use the only non-removable disk; fail if multiple found
//	largest         — largest non-removable disk (explicit opt-in)
//	smallest        — smallest non-removable disk (typical OS-on-SSD pattern)
//	nvme            — the only NVMe disk; fail if multiple found
//	/dev/sda (etc.) — exact device path; fail if not present
func resolveDisk(target string) (install.Disk, error) {
	disks, err := candidateDisks()
	if err != nil {
		return install.Disk{}, err
	}
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "", "auto":
		if len(disks) != 1 {
			return install.Disk{}, fmt.Errorf(
				"expected exactly one disk, found %d — set SPINIFEX_DISK to one of:\n%s\n"+
					"  largest   (largest disk)\n"+
					"  smallest  (smallest disk)\n"+
					"  nvme      (NVMe only)\n"+
					"  /dev/sdX  (exact path)",
				len(disks), diskList(disks))
		}
		return disks[0], nil
	case "largest":
		return pickBySize(disks, true)
	case "smallest":
		return pickBySize(disks, false)
	case "nvme":
		nvmes := slices.DeleteFunc(slices.Clone(disks), func(d install.Disk) bool {
			return !strings.HasPrefix(filepath.Base(d.Path), "nvme")
		})
		if len(nvmes) != 1 {
			return install.Disk{}, fmt.Errorf("expected exactly one NVMe disk, found %d:\n%s",
				len(nvmes), diskList(nvmes))
		}
		return nvmes[0], nil
	default:
		found, err := disksByName([]string{target})
		if err != nil {
			return install.Disk{}, err
		}
		return found[0], nil
	}
}

// candidateDisks lists the disks an unattended install may erase: fixed, and
// not the installer's own media.
func candidateDisks() ([]install.Disk, error) {
	all, err := listDisks()
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(d install.Disk) bool {
		return d.Removable || d.LiveMedia
	}), nil
}

func pickBySize(disks []install.Disk, largest bool) (install.Disk, error) {
	if len(disks) == 0 {
		return install.Disk{}, fmt.Errorf("no non-removable disks found")
	}
	sorted := slices.Clone(disks)
	slices.SortFunc(sorted, func(a, b install.Disk) int { return cmp.Compare(a.Bytes, b.Bytes) })
	if largest {
		return sorted[len(sorted)-1], nil
	}
	return sorted[0], nil
}

// diskList renders candidates for an error message.
func diskList(disks []install.Disk) string {
	lines := make([]string, len(disks))
	for i, d := range disks {
		lines[i] = fmt.Sprintf("  %s (%s, %s)", d.Path, d.SizeHuman(), d.Content)
	}
	return strings.Join(lines, "\n")
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
