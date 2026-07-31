package install

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/cmd/installer/firstboot"
	"github.com/mulgadc/spinifex/cmd/installer/systemd"
)

// mountRoot is the install target. A variable, not a constant, so tests can
// drive the file-writing steps against a temp tree rather than a real mount.
var mountRoot = "/mnt/spinifex-install"

// efiPart is the ESP mount point inside the target. Derived on each call so it
// cannot go stale against mountRoot.
func efiPart() string { return filepath.Join(mountRoot, "boot/efi") }

// step is one named unit of work in the install sequence.
type step struct {
	name string
	fn   func() error
}

// Run executes all installation steps in order. It is intentionally sequential
// and explicit — each step is visible in logs so failures are easy to diagnose.
func Run(cfg *Config) error {
	// Re-checked here even though the UI and the headless path both validate:
	// this is the last point before anything is erased, and it is the only one
	// every caller must pass through.
	if err := cfg.Storage.Validate(); err != nil {
		return fmt.Errorf("storage: %w", err)
	}

	// The live environment may not have /sbin or /usr/sbin in PATH. Set it
	// explicitly so exec.Command's LookPath finds system binaries like grub-install.
	_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

	// Unmount unconditionally on exit so a failed step never leaves the target
	// mounted in the live environment, which would make a retry double-mount or,
	// on ZFS, fail with "pool is busy".
	defer cleanupTarget(cfg.Storage)

	steps := []step{{"partition disks", func() error { return partitionDisks(cfg.Storage) }}}
	if cfg.Storage.FS.IsZFS() {
		steps = append(steps, zfsRootSteps(cfg)...)
	} else {
		steps = append(steps, ext4RootSteps(cfg)...)
	}
	steps = append(steps,
		step{"copy rootfs", func() error { return copyRootfs(cfg.Storage) }},
		step{"create swap", func() error { return createSwap(cfg.Storage) }},
		step{"write fstab", func() error { return writeFstab(cfg.Storage) }},
		step{"install spinifex", func() error { return installSpinifex(cfg) }},
		step{"write network config", func() error { return writeNetworkConfig(cfg) }},
		step{"write firstboot service", func() error { return firstboot.Write(mountRoot, cfg.toFirstbootConfig()) }},
		step{"install bootloader", func() error { return installBootloader(cfg.Storage) }},
		step{"install CA cert", func() error { return installCACert(cfg) }},
	)

	for _, s := range steps {
		slog.Info("installer", "step", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("step %q: %w", s.name, err)
		}
	}

	slog.Info("installation complete")
	refreshBootToolNote(cfg.Storage)
	fireInstallCallback()
	promptRemoveUSB()
	return reboot()
}

// ext4RootSteps formats and mounts a single-disk ext4 root.
func ext4RootSteps(cfg *Config) []step {
	return []step{
		{"format partitions", func() error { return formatPartitions(cfg.Storage) }},
		{"mount partitions", func() error { return mountPartitions(cfg.Storage) }},
	}
}

// zfsRootSteps builds the root pool. The ESPs are formatted here rather than
// with the bootloader so that a failure to make a filesystem on one of them is
// reported before the rootfs has been copied.
func zfsRootSteps(cfg *Config) []step {
	// Resolved once, before any step runs, so the values written to the pool,
	// to modprobe.d and to the daemon's reserve are all the same numbers.
	var opts ZFSOpts
	return []step{
		{"format ESPs", func() error { return formatESPs(cfg.Storage) }},
		{"load zfs module", loadZFSModule},
		{"create zfs pool", func() error {
			opts = resolveZFSOpts(cfg.Storage)
			slog.Info("zfs pool", "topology", cfg.Storage.FS, "disks", len(cfg.Storage.Disks),
				"ashift", opts.Ashift, "compress", opts.Compress, "arc_max_mib", opts.ARCMaxMiB)
			return createPool(cfg.Storage, opts)
		}},
		{"create zfs datasets", createDatasets},
		{"configure zfs", func() error { return writeZFSSystemConfig(opts) }},
	}
}

// cleanupTarget releases everything the install mounted, in the right order for
// the filesystem in use.
func cleanupTarget(cfg DiskConfig) {
	unbindChrootMounts()
	if cfg.FS.IsZFS() {
		// Recursive: the layout is a dozen nested datasets, and leaving one
		// mounted is enough to block the export.
		_ = runQuiet("umount", "-R", mountRoot)
		exportPool()
		return
	}
	_ = runQuiet("umount", efiPart())
	_ = runQuiet("umount", mountRoot)
}

func formatPartitions(cfg DiskConfig) error {
	if err := formatESPs(cfg); err != nil {
		return err
	}
	return run("mkfs.ext4", "-F", cfg.Primary().PartitionPath(rootPartNum))
}

func mountPartitions(cfg DiskConfig) error {
	d := cfg.Primary()
	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		return err
	}
	if err := run("mount", d.PartitionPath(rootPartNum), mountRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(efiPart(), 0o755); err != nil {
		return err
	}
	return run("mount", d.PartitionPath(espPartNum), efiPart())
}

// copyRootfs copies the live squashfs environment onto the target disk using
// rsync. This is the air-gapped alternative to debootstrap — all packages are
// already embedded in the ISO so no network access is required.
func copyRootfs(cfg DiskConfig) error {
	args := []string{
		"-aHAX", "--delete", "--info=progress2",
		"--exclude=/proc",
		"--exclude=/sys",
		"--exclude=/dev",
		"--exclude=/run",
		"--exclude=/tmp",
		"--exclude=/cdrom",
		"--exclude=/mnt",
		"--exclude=/etc/machine-id",
		"--exclude=/var/lib/dbus/",
		"--exclude=/etc/openvswitch/",
		"--exclude=/var/lib/openvswitch/",
		"--exclude=/var/lib/dhcpcd/",
		"--exclude=/etc/ssh/ssh_host_*",
		"--exclude=/lost+found",
		"--exclude=/boot/efi",
	}
	args = append(args, datasetProtectFilters(cfg)...)
	args = append(args, "/", mountRoot+"/")
	if err := run("rsync", args...); err != nil {
		return err
	}

	// Verify critical paths exist before proceeding. rsync exits 0 on ENOSPC
	// for individual file writes on some filesystems, which would produce a
	// partial rootfs that boots into a panic.
	critical := []string{
		filepath.Join(mountRoot, "bin/bash"),
		filepath.Join(mountRoot, "lib/systemd/systemd"),
		filepath.Join(mountRoot, "usr/local/bin/spx"),
	}
	for _, p := range critical {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("copyRootfs: critical path missing after rsync (%s): %w", p, err)
		}
	}

	// rsync skips excluded paths entirely — recreate the virtual filesystem
	// mount points that systemd expects to exist on the installed system.
	mountPoints := []struct {
		path string
		mode os.FileMode
	}{
		{"proc", 0o555},
		{"sys", 0o555},
		{"dev", 0o755},
		{"run", 0o755},
		{"run/lock", 0o1777},
		{"tmp", 0o1777},
		{"mnt", 0o755},
	}
	for _, mp := range mountPoints {
		if err := os.MkdirAll(filepath.Join(mountRoot, mp.path), mp.mode); err != nil {
			return fmt.Errorf("create mountpoint /%s: %w", mp.path, err)
		}
	}
	return nil
}

func installSpinifex(cfg *Config) error {
	// The rootfs copy already contains spx and spinifex-installer at
	// /usr/local/bin/ — no binary copy needed. Regenerate machine-specific
	// identity files so each installed node is unique.

	// Bind-mount /dev, /proc, /sys into the chroot so PAM (chpasswd) and
	// other chroot commands work correctly. Trixie's PAM requires /proc and
	// /dev/urandom for password hashing.
	if err := bindChrootMounts(); err != nil {
		return err
	}
	defer unbindChrootMounts()

	// Generate a unique machine-id from the kernel CSPRNG. We do not use
	// systemd-machine-id-setup in the chroot: it reads the SMBIOS UUID via
	// the bind-mounted /sys, which is identical on cloned VMs or hosts with
	// absent/zeroed DMI data, and falls back to non-unique sources (MAC,
	// hostname) when that also fails — producing the same ID on every node.
	// /proc/sys/kernel/random/uuid is always unique and requires no chroot.
	machineIDPath := filepath.Join(mountRoot, "etc/machine-id")
	rawUUID, err := os.ReadFile("/proc/sys/kernel/random/uuid")
	if err != nil {
		return fmt.Errorf("installSpinifex: read kernel uuid: %w", err)
	}
	machineID := strings.ReplaceAll(strings.TrimSpace(string(rawUUID)), "-", "") + "\n"
	if err := os.WriteFile(machineIDPath, []byte(machineID), 0o444); err != nil {
		return fmt.Errorf("installSpinifex: write machine-id: %w", err)
	}
	// dbus mirrors /etc/machine-id; remove it so it is re-created from the
	// new ID on first boot rather than carrying a stale value.
	_ = os.Remove(filepath.Join(mountRoot, "var/lib/dbus/machine-id"))

	// Hostname.
	hostnamePath := filepath.Join(mountRoot, "etc/hostname")
	if err := os.WriteFile(hostnamePath, []byte(cfg.Hostname+"\n"), 0o644); err != nil {
		return err
	}

	// /etc/hosts entry for the hostname.
	hosts := fmt.Sprintf("127.0.0.1\tlocalhost\n127.0.1.1\t%s\n", cfg.Hostname)
	if err := os.WriteFile(filepath.Join(mountRoot, "etc/hosts"), []byte(hosts), 0o644); err != nil {
		return err
	}

	// Set root + spinifex passwords. We invoke chpasswd from the LIVE
	// installer (not via `chroot`), passing the target root with -R and
	// forcing -c YESCRYPT. This deliberately bypasses PAM:
	//   * `chroot ... chpasswd` uses /etc/pam.d/chpasswd → common-password →
	//     pam_unix.so with the "obscure" option, which can return
	//     "Authentication token manipulation error" inside a chroot for
	//     reasons that are awkward to diagnose (audit subsystem, locked
	//     shadow entries from useradd, etc.).
	//   * -c YESCRYPT tells chpasswd to hash locally with libcrypt and write
	//     directly to <root>/etc/shadow — no PAM stack involved. YESCRYPT
	//     matches Trixie's ENCRYPT_METHOD so subsequent logins use the same
	//     algorithm.
	//   * -R <root> opens the target's passwd/shadow directly; no bind
	//     mounts of /dev/urandom or /proc are needed for the password step.
	if cfg.RootPassword != "" {
		if err := setShadowPassword("root", cfg.RootPassword); err != nil {
			return fmt.Errorf("set root password: %w", err)
		}
		// The spinifex account is the default interactive login on the
		// node (console + SSH). Root SSH is disabled, so this is the sole
		// remote entry point. The user itself is created at rootfs build
		// time (build-rootfs.sh) — here we just set its password.
		if err := setShadowPassword("spinifex", cfg.RootPassword); err != nil {
			return fmt.Errorf("set spinifex password: %w", err)
		}
	}

	// Write /etc/spinifex/node.conf — read at runtime by spx admin banner
	// to look up the current IP dynamically (handles IP changes after install).
	// MANAGEMENT_IFACE is the bridge (br-wan), not the physical NIC.
	// MANAGEMENT_IP is empty for DHCP — banner's --boot-check fills it in at boot.
	nodeConf := fmt.Sprintf("MANAGEMENT_IP=%s\nMANAGEMENT_IFACE=br-wan\nNODE_HOSTNAME=%s\n",
		cfg.WAN.Address, cfg.Hostname)
	confDir := filepath.Join(mountRoot, "etc/spinifex")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(confDir, "node.conf"), []byte(nodeConf), 0o644); err != nil {
		return err
	}

	// dhcpcd-base is present on the installed system (used by setup-ovn.sh
	// for WAN DHCP acquisition). Mask the standalone dhcpcd.service so it
	// never auto-starts and races with systemd-networkd's built-in DHCP
	// client on br-wan.
	if err := maskSystemdUnit(mountRoot, "dhcpcd.service"); err != nil {
		slog.Warn("installSpinifex: failed to mask dhcpcd.service", "err", err)
	}

	return nil
}

func writeNetworkConfig(cfg *Config) error {
	// IPs live on Linux bridges (br-wan, br-lan), not on the physical NICs.
	// systemd-networkd owns the full lifecycle: it creates the bridge NetDevs,
	// enslaves the physical NICs, and runs the DHCP client. This means the
	// veth pair that setup-ovn.sh adds later (veth-wan-br) is a port of a
	// networkd-known bridge, so a networkctl reload or reboot never orphans it.
	netdDir := filepath.Join(mountRoot, "etc/systemd/network")
	if err := os.MkdirAll(netdDir, 0o755); err != nil {
		return err
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("network roles: %w", err)
	}

	// A physical NIC is configured once even when two roles share it via
	// VLANs, so parent-interface files are keyed by NIC rather than by plane.
	parents := map[string]*parentNIC{}
	for _, rb := range cfg.Roles() {
		p, ok := parents[rb.Role.Interface]
		if !ok {
			p = &parentNIC{}
			parents[rb.Role.Interface] = p
		}
		if rb.Role.VLAN > 0 {
			p.vlans = append(p.vlans, rb.Role.Link())
		} else {
			p.bridge = rb.Plane.Bridge()
		}
		// The parent must carry at least the largest MTU of any VLAN riding it.
		if rb.Role.MTU > p.mtu {
			p.mtu = rb.Role.MTU
		}
	}
	for iface, p := range parents {
		if err := writeParentNIC(netdDir, iface, p); err != nil {
			return err
		}
	}

	for _, rb := range cfg.Roles() {
		// Only br-wan auto-activates; the east-west bridges are brought up by
		// their own unit after network-online.target so a missing cable or a
		// DHCP timeout on them can never stall the management path.
		manual := rb.Plane != PlaneWAN
		if err := writeNetworkdBridge(netdDir, rb.Plane, rb.Role, manual); err != nil {
			return err
		}
		if manual {
			if err := systemd.WriteBridgeUnit(mountRoot, rb.Plane.Bridge()); err != nil {
				return fmt.Errorf("%s bridge unit: %w", rb.Plane, err)
			}
		}
	}

	// Unmask systemd-networkd-wait-online on the installed system and scope
	// it to br-wan only. The live ISO masks this service (build-rootfs.sh) to
	// avoid blocking the installer environment (which has no br-wan). The mask
	// is copied to the installed system by copyRootfs and must be removed here
	// so that network-online.target — and therefore spinifex-firstboot.service
	// (After=network-online.target) — does not fire before br-wan has its
	// DHCP lease and default route. Without this, setup-ovn.sh finds no default
	// route and exits early, creating no veth pair.
	//
	// The --interface=br-wan scope means br-lan (ActivationPolicy=manual) never
	// blocks the wait; --timeout=60 caps the delay on a cold switch.
	waitOnlineMask := filepath.Join(mountRoot, "etc/systemd/system/systemd-networkd-wait-online.service")
	_ = os.Remove(waitOnlineMask)
	waitOnlineDir := filepath.Join(mountRoot, "etc/systemd/system/systemd-networkd-wait-online.service.d")
	if err := os.MkdirAll(waitOnlineDir, 0o755); err != nil {
		return err
	}
	waitOnlineConf := "[Service]\nExecStart=\nExecStart=/lib/systemd/systemd-networkd-wait-online --interface=br-wan --timeout=60\n"
	if err := os.WriteFile(filepath.Join(waitOnlineDir, "spinifex-wan-only.conf"), []byte(waitOnlineConf), 0o644); err != nil {
		return err
	}

	// Disable IPv6 via sysctl — belt-and-suspenders alongside IPv6AcceptRA=no
	// in the networkd .network files.
	var bridges []string
	for _, rb := range cfg.Roles() {
		bridges = append(bridges, rb.Plane.Bridge())
	}
	var sysctl strings.Builder
	sysctl.WriteString("# Generated by Spinifex installer — IPv6 disabled on management bridges\n")
	for _, br := range bridges {
		fmt.Fprintf(&sysctl, "net.ipv6.conf.%s.disable_ipv6=1\n", br)
	}
	sysctlDir := filepath.Join(mountRoot, "etc/sysctl.d")
	if err := os.MkdirAll(sysctlDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sysctlDir, "99-spinifex-network.conf"), []byte(sysctl.String()), 0o644); err != nil {
		return err
	}

	// Pin each NIC name to its MAC address via udev so the installed system
	// always uses the same interface name regardless of probe order.
	udevDir := filepath.Join(mountRoot, "etc/udev/rules.d")
	if err := os.MkdirAll(udevDir, 0o755); err != nil {
		return err
	}
	var udevRules strings.Builder
	pinned := map[string]bool{}
	for _, rb := range cfg.Roles() {
		iface := rb.Role.Interface
		// Two roles can share one NIC via VLANs; pin the physical name once.
		if iface == "" || pinned[iface] {
			continue
		}
		pinned[iface] = true
		mac, err := os.ReadFile("/sys/class/net/" + iface + "/address")
		if err != nil {
			slog.Warn("writeNetworkConfig: could not read NIC MAC, skipping udev pin", "iface", iface, "err", err)
			continue
		}
		fmt.Fprintf(&udevRules, "SUBSYSTEM==\"net\", ACTION==\"add\", ATTR{address}==\"%s\", NAME=\"%s\"\n",
			strings.TrimSpace(string(mac)), iface)
	}
	if udevRules.Len() > 0 {
		return os.WriteFile(filepath.Join(udevDir, "70-spinifex-net.rules"), []byte(udevRules.String()), 0o644)
	}
	return nil
}

// parentNIC accumulates how one physical interface is used across roles. A NIC
// is either enslaved directly to a bridge (untagged) or carries VLAN
// subinterfaces — never both for the same plane.
type parentNIC struct {
	bridge string
	vlans  []string
	mtu    int
}

// writeParentNIC writes the .network file for a physical interface. An
// untagged NIC is enslaved straight to its bridge; a tagged one declares its
// VLAN subinterfaces and stays unbridged, since the VLAN devices are what get
// enslaved.
func writeParentNIC(dir, iface string, p *parentNIC) error {
	var b strings.Builder
	fmt.Fprintf(&b, "[Match]\nName=%s\n\n", iface)
	// A physical NIC here is either a bridge port or a VLAN trunk, and neither
	// carries the address — the bridge above it does. Such a link never reaches
	// the "degraded" state systemd-networkd-wait-online requires, so leaving it
	// required makes that unit fail on every boot and stall until it times out.
	b.WriteString("[Link]\nRequiredForOnline=no\n")
	if p.mtu > 0 {
		fmt.Fprintf(&b, "MTUBytes=%d\n", p.mtu)
	}
	b.WriteString("\n[Network]\n")
	if p.bridge != "" {
		fmt.Fprintf(&b, "Bridge=%s\n", p.bridge)
	}
	for _, v := range p.vlans {
		fmt.Fprintf(&b, "VLAN=%s\n", v)
	}
	b.WriteString("IPv6AcceptRA=no\n")
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("05-spinifex-nic-%s.network", iface)), []byte(b.String()), 0o644)
}

// writeNetworkdBridge writes the systemd-networkd files that put a plane's
// address on its own Linux bridge:
//
//   - 1{n}-spinifex-{plane}-vlan.netdev   VLAN subinterface (tagged roles only)
//   - 1{n}-spinifex-{plane}-vlan.network  enslaves the VLAN device to the bridge
//   - 2{n}-spinifex-{plane}.netdev        declares the bridge device
//   - 2{n}-spinifex-{plane}.network       configures IP on the bridge
//
// The physical NIC itself is written separately by writeParentNIC, because two
// roles may share one NIC and it must be configured exactly once.
//
// manual=true sets ActivationPolicy=manual so networkd creates the bridge but
// does not auto-activate it — used for br-lan and br-vpc, which their own
// spinifex-{plane}-bridge.service activates after network-online.target.
func writeNetworkdBridge(dir string, plane Plane, role NetworkRole, manual bool) error {
	name := string(plane)
	bridgeName := plane.Bridge()

	if role.VLAN > 0 {
		link := role.Link()
		vlanNetdev := fmt.Sprintf("[NetDev]\nName=%s\nKind=vlan\n\n[VLAN]\nId=%d\n", link, role.VLAN)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("10-spinifex-%s-vlan.netdev", name)), []byte(vlanNetdev), 0o644); err != nil {
			return err
		}
		var v strings.Builder
		fmt.Fprintf(&v, "[Match]\nName=%s\n\n", link)
		// Enslaved to the bridge below, so it is not what "online" means here.
		v.WriteString("[Link]\nRequiredForOnline=no\n")
		if role.MTU > 0 {
			fmt.Fprintf(&v, "MTUBytes=%d\n", role.MTU)
		}
		fmt.Fprintf(&v, "\n[Network]\nBridge=%s\nIPv6AcceptRA=no\n", bridgeName)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("11-spinifex-%s-vlan.network", name)), []byte(v.String()), 0o644); err != nil {
			return err
		}
	}

	// Without a policy systemd-networkd invents a MAC for the bridge, so the node
	// appears on the wire as an address that belongs to no NIC — the DHCP server
	// sees a request whose source does not match its own chaddr and the unicast
	// reply goes nowhere. "none" leaves the kernel to inherit the enslaved port's
	// address, which is what a bridged uplink is supposed to present, and keeps
	// it stable across boots for switch MAC tables and DHCP reservations.
	brNetdev := fmt.Sprintf("[NetDev]\nName=%s\nKind=bridge\nMACAddressPolicy=none\n", bridgeName)
	if role.MTU > 0 {
		brNetdev += fmt.Sprintf("MTUBytes=%d\n", role.MTU)
	}
	brNetdev += "\n[Bridge]\nSTP=no\n"
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("20-spinifex-%s.netdev", name)), []byte(brNetdev), 0o644); err != nil {
		return err
	}

	// Bridge .network — IP configuration.
	var b strings.Builder
	fmt.Fprintf(&b, "[Match]\nName=%s\n\n", bridgeName)
	if manual {
		b.WriteString("[Link]\nActivationPolicy=manual\nRequiredForOnline=no\n\n")
	}
	b.WriteString("[Network]\n")
	if role.DHCPMode {
		b.WriteString("DHCP=ipv4\n")
	} else {
		cidr, err := addrCIDR(role.Address, role.Mask)
		if err != nil {
			return fmt.Errorf("bridge %s: %w", bridgeName, err)
		}
		fmt.Fprintf(&b, "Address=%s\n", cidr)
		// Only the wan plane installs a default route; the east-west planes
		// are link-local to the rack.
		if role.Gateway != "" && plane == PlaneWAN {
			fmt.Fprintf(&b, "Gateway=%s\n", role.Gateway)
		}
		// Resolvers follow the default route for the same reason. A resolver
		// pinned to an east-west bridge is unreachable — those bridges are
		// link-local and activate manually — but resolved would still try it,
		// putting a timeout in front of every lookup on the node.
		if plane == PlaneWAN {
			for _, ns := range role.DNS {
				if ns = strings.TrimSpace(ns); ns != "" {
					fmt.Fprintf(&b, "DNS=%s\n", ns)
				}
			}
		}
	}
	b.WriteString("IPv6AcceptRA=no\nConfigureWithoutCarrier=yes\n")
	if manual && role.DHCPMode {
		// The east-west bridges are non-critical; fail fast on DHCP so a
		// missing cable does not stall their activation unit indefinitely.
		b.WriteString("\n[DHCP]\nTimeout=10\n")
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("20-spinifex-%s.network", name)), []byte(b.String()), 0o644); err != nil {
		return err
	}

	if role.WiFiSSID != "" {
		if err := writeWPASupplicant(role.Interface, role.WiFiSSID, role.WiFiPass); err != nil {
			return err
		}
	}
	return nil
}

// addrCIDR converts a dotted-decimal address + subnet mask to CIDR notation.
func addrCIDR(addr, dotMask string) (string, error) {
	if net.ParseIP(addr) == nil {
		return "", fmt.Errorf("invalid IP: %s", addr)
	}
	maskIP := net.ParseIP(dotMask)
	if maskIP == nil {
		return "", fmt.Errorf("invalid mask: %s", dotMask)
	}
	m := net.IPMask(maskIP.To4())
	ones, bits := m.Size()
	if bits == 0 {
		return "", fmt.Errorf("non-contiguous or zero subnet mask: %s", dotMask)
	}
	return fmt.Sprintf("%s/%d", addr, ones), nil
}

// writeWPASupplicant writes a wpa_supplicant config for nicIface and enables
// the per-interface wpa_supplicant@{iface}.service so authentication completes
// before networkd enslaves the NIC to the bridge.
func writeWPASupplicant(nicIface, ssid, psk string) error {
	conf := fmt.Sprintf(
		"ctrl_interface=DIR=/run/wpa_supplicant GROUP=netdev\nupdate_config=1\n\nnetwork={\n\tssid=%q\n\tpsk=%q\n}\n",
		ssid, psk,
	)
	wpaDir := filepath.Join(mountRoot, "etc/wpa_supplicant")
	if err := os.MkdirAll(wpaDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(wpaDir, "wpa_supplicant-"+nicIface+".conf"), []byte(conf), 0o600); err != nil {
		return err
	}
	// Enable via symlink into multi-user.target.wants pointing at the
	// package-provided template unit.
	wantsDir := filepath.Join(mountRoot, "etc/systemd/system/multi-user.target.wants")
	if err := os.MkdirAll(wantsDir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(wantsDir, "wpa_supplicant@"+nicIface+".service")
	_ = os.Remove(link)
	return os.Symlink("/lib/systemd/system/wpa_supplicant@.service", link)
}

func installBootloader(cfg DiskConfig) error {
	// Branding assets have to be in place before either path regenerates
	// grub.cfg — the ZFS path copies them out of /boot onto each ESP.
	copySplashImage(mountRoot)
	copyGrubFont(mountRoot)

	if cfg.FS.IsZFS() {
		// The initramfs must be able to import the pool before the ESPs are
		// synced, since refresh copies whatever initrd exists at that moment.
		if err := buildZFSInitramfs(); err != nil {
			return err
		}
		return installBootToolZFS(cfg)
	}
	return installBootloaderExt4(cfg.Primary())
}

// buildZFSInitramfs regenerates the target's initramfs so it carries the ZFS
// module, the pool cache and the hostid. The image copied from the live ISO was
// built for a machine with an ext4 root and cannot import a pool.
func buildZFSInitramfs() error {
	// ZFS refuses to import a pool last touched by a different host without
	// -f, and the hostid the initramfs sees must match the one recorded on the
	// pool — otherwise every boot stops in the initramfs shell.
	if err := run("cp", "-f", "/etc/hostid", filepath.Join(mountRoot, "etc/hostid")); err != nil {
		slog.Warn("could not copy /etc/hostid, generating one in the target", "err", err)
		if err := run("zgenhostid", "-f", "-o", filepath.Join(mountRoot, "etc/hostid")); err != nil {
			return fmt.Errorf("zgenhostid: %w", err)
		}
	}

	// The pool was created with cachefile=none so the live environment would
	// not claim it. The installed system needs the cache to import at boot
	// without scanning every block device.
	cacheDir := filepath.Join(mountRoot, "etc/zfs")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	if err := run("zpool", "set", "cachefile="+filepath.Join(cacheDir, "zpool.cache"), ZFSPoolName); err != nil {
		return fmt.Errorf("set zpool cachefile: %w", err)
	}

	if err := bindChrootMounts(); err != nil {
		return err
	}
	defer unbindChrootMounts()
	if err := run("chroot", mountRoot, "update-initramfs", "-u", "-k", "all"); err != nil {
		return fmt.Errorf("update-initramfs: %w", err)
	}
	return nil
}

func installBootloaderExt4(disk Disk) error {
	// grub-install runs in the live environment (not chroot) using the
	// grub-pc-bin and grub-efi-amd64-bin packages already present on the ISO.
	// --boot-directory points at the installed system's /boot.
	bootDir := filepath.Join(mountRoot, "boot")
	efiDir := filepath.Join(mountRoot, "boot", "efi")

	efiErr := run("grub-install",
		"--target=x86_64-efi",
		"--efi-directory="+efiDir,
		"--boot-directory="+bootDir,
		"--bootloader-id=spinifex",
		"--removable",
		"--recheck",
	)
	if efiErr != nil {
		slog.Warn("installBootloader: EFI install failed", "err", efiErr)
	}
	if biosErr := run("grub-install",
		"--target=i386-pc",
		"--boot-directory="+bootDir,
		"--recheck",
		disk.Path,
	); biosErr != nil {
		if efiErr != nil {
			// Both targets failed — the system will not boot.
			return fmt.Errorf("both bootloader targets failed (EFI: %w; BIOS: %w)", efiErr, biosErr)
		}
		return biosErr
	}

	if err := writeGrubDefaults(DiskConfig{FS: FSExt4}); err != nil {
		return err
	}

	if err := bindChrootMounts(); err != nil {
		return err
	}
	defer unbindChrootMounts()
	return run("chroot", mountRoot, "update-grub")
}

func installCACert(cfg *Config) error {
	if !cfg.HasCACert || cfg.CACert == "" {
		return nil
	}
	certPath := filepath.Join(mountRoot, "usr/local/share/ca-certificates/spinifex-ca.crt")
	if err := os.WriteFile(certPath, []byte(cfg.CACert), 0o644); err != nil {
		return err
	}
	if err := bindChrootMounts(); err != nil {
		return err
	}
	defer unbindChrootMounts()
	return run("chroot", mountRoot, "update-ca-certificates")
}

// promptRemoveUSB prints a removal reminder and waits up to 10 seconds for
// the user to press Enter before rebooting. Reading from os.Stdin works because
// spinifex-init redirects the installer's stdin from $CONSOLE_DEV.
func promptRemoveUSB() {
	fmt.Println("\n\033[1mInstallation complete.\033[0m")
	fmt.Println("Remove the USB drive, then press Enter to reboot (auto-rebooting in 10 seconds)...")

	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

// fireInstallCallback notifies the boot controller that installation is done
// so it clears the PXE install flag before the node reboots. Without this,
// a PXE-first boot order causes the node to reinstall on every reboot until
// firstboot fires the callback — which it can never do if it never runs.
// No-op when SPINIFEX_INSTALL_CALLBACK is not set (ISO/USB installs).
func fireInstallCallback() {
	url := strings.TrimSpace(os.Getenv("SPINIFEX_INSTALL_CALLBACK"))
	if url == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url) //nolint:noctx // installer has no context; best-effort fire-and-forget
	if err != nil {
		slog.Warn("fireInstallCallback: request failed", "url", url, "err", err)
		return
	}
	resp.Body.Close()
	slog.Info("fireInstallCallback: notified boot controller", "url", url, "status", resp.StatusCode)
}

func reboot() error {
	// sync filesystems before reboot so nothing is lost.
	_ = run("sync")
	// Use the kernel syscall directly — the live environment runs spinifex-init
	// as PID 1 (not systemd), so the reboot(8) utility fails trying to reach D-Bus.
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}

// toFirstbootConfig maps installer Config to the firstboot package's Config.
func (c *Config) toFirstbootConfig() firstboot.Config {
	// Both addresses come from their plane after collapsing, so a single-NIC
	// node resolves them to the wan address and a three-plane rack keeps
	// Geneve on vpc and cluster traffic on lan. Empty for DHCP — setup-ovn.sh
	// auto-detects the IP from the default route at boot in that case.
	return firstboot.Config{
		Hostname:        c.Hostname,
		EncapIP:         c.PlaneAddress(PlaneVPC),
		LANIP:           c.PlaneAddress(PlaneLAN),
		WANIP:           c.PlaneAddress(PlaneWAN),
		ClusterRole:     c.ClusterRole,
		JoinAddr:        c.JoinAddr,
		Email:           c.Email,
		InstallCallback: strings.TrimSpace(os.Getenv("SPINIFEX_INSTALL_CALLBACK")),
		SkipFormation:   c.SkipFormation,
	}
}

// writeFstab writes /etc/fstab on the installed system.
//
// On ZFS the root and every dataset are mounted by ZFS itself from properties
// stored on the pool, and the ESPs are mounted on demand by spinifex-boot-tool,
// so the file holds nothing but swap.
func writeFstab(cfg DiskConfig) error {
	fstab := "# /etc/fstab — generated by Spinifex installer\n"

	if !cfg.FS.IsZFS() {
		d := cfg.Primary()
		rootUUID, err := partUUID(d.PartitionPath(rootPartNum))
		if err != nil {
			return fmt.Errorf("get root UUID: %w", err)
		}
		efiUUID, err := partUUID(d.PartitionPath(espPartNum))
		if err != nil {
			return fmt.Errorf("get EFI UUID: %w", err)
		}
		fstab += fmt.Sprintf("UUID=%s / ext4 errors=remount-ro 0 1\nUUID=%s /boot/efi vfat umask=0077 0 1\n",
			rootUUID, efiUUID)
	}

	// Only claim the swap the previous step actually produced; systemd fails the
	// boot on a missing swap unit rather than ignoring the line.
	switch {
	case cfg.FS.IsZFS():
		if _, err := os.Stat("/dev/zvol/" + swapZvolName); err == nil {
			fstab += fmt.Sprintf("/dev/zvol/%s none swap discard 0 0\n", swapZvolName)
		}
	default:
		if _, err := os.Stat(filepath.Join(mountRoot, swapFileName)); err == nil {
			fstab += fmt.Sprintf("/%s none swap sw 0 0\n", swapFileName)
		}
	}
	return os.WriteFile(filepath.Join(mountRoot, "etc/fstab"), []byte(fstab), 0o644)
}

const (
	swapFileName = "swapfile"
	// Nodes below this get half their RAM, which keeps a 4 GiB test VM to a
	// 2 GiB file instead of eating its disk. At or above it the machine is a
	// real host with a real disk, so swap is worth sizing for large imports.
	swapRAMThreshold = 32 << 30
	swapLargeBytes   = 64 << 30
	// Below this there is not enough to be worth the space it costs.
	swapMinBytes = 512 << 20
	// Hard ceiling as a share of free disk, whatever RAM suggests: a 64 GiB file
	// must never fill a small root filesystem.
	swapDiskShare = 4
)

// createSwapFile lays down /swapfile on the installed root, sized to the disk.
//
// Importing an image streams the decompressed disk through viperblock into
// predastore, which a 2-4 GiB node cannot hold in RAM — the kernel OOM-kills the
// import mid-flush. Swap trades speed for completing the operation at all. It is
// a stopgap for the memory profile of that path, not a fix for it.
//
// Failure is logged and not fatal: a node that boots without swap is worse at
// large imports but is otherwise fine, and losing a whole install over it would
// be the greater harm.
// swapZvolName is the swap volume on a ZFS root. ZFS cannot host a swap *file*
// at all, so a zvol is the only option — the same one Proxmox uses.
const swapZvolName = ZFSPoolName + "/swap"

// createSwap lays down swap in whichever form the root filesystem supports.
func createSwap(cfg DiskConfig) error {
	if cfg.FS.IsZFS() {
		return createSwapZvol()
	}
	return createSwapFile()
}

// createSwapZvol creates the swap volume on a ZFS root.
//
// The properties are not tuning preferences. A zvol swapping under memory
// pressure can deadlock against the ARC, and these are the settings that make
// it survivable: page-sized blocks so a swap-out is one record, no data
// caching so swap pages are never copied into the ARC, and sync writes so a
// page is on disk before the kernel frees it.
func createSwapZvol() error {
	size, err := swapSize(mountRoot)
	if err != nil {
		slog.Warn("swap: cannot size swap volume, continuing without one", "err", err)
		return nil
	}
	if size == 0 {
		slog.Warn("swap: pool too small for a useful swap volume, continuing without one")
		return nil
	}

	if err := run("zfs", "create",
		"-V", strconv.FormatInt(size, 10),
		"-b", strconv.Itoa(os.Getpagesize()),
		"-o", "logbias=throughput",
		"-o", "sync=always",
		"-o", "primarycache=metadata",
		"-o", "secondarycache=none",
		"-o", "compression=zle",
		"-o", "com.sun:auto-snapshot=false",
		swapZvolName,
	); err != nil {
		slog.Warn("swap: could not create swap volume, continuing without swap", "err", err)
		return nil
	}

	dev := "/dev/zvol/" + swapZvolName
	if err := waitForPath(dev, time.Now().Add(10*time.Second)); err != nil {
		slog.Warn("swap: zvol device node never appeared, continuing without swap", "err", err)
		return nil
	}
	if err := run("mkswap", "-f", dev); err != nil {
		slog.Warn("swap: mkswap failed, continuing without swap", "err", err)
		return nil
	}
	slog.Info("swap: created", "zvol", swapZvolName, "bytes", size)
	return nil
}

func createSwapFile() error {
	size, err := swapSize(mountRoot)
	if err != nil {
		slog.Warn("swap: cannot size swap file, continuing without one", "err", err)
		return nil
	}
	if size == 0 {
		slog.Warn("swap: disk too small for a useful swap file, continuing without one",
			"min_bytes", swapMinBytes)
		return nil
	}

	path := filepath.Join(mountRoot, swapFileName)
	// fallocate rather than dd: on ext4 it reserves real extents, so mkswap and
	// swapon accept it, and a 32 GiB file costs no write time during install.
	if err := run("fallocate", "-l", strconv.FormatInt(size, 10), path); err != nil {
		slog.Warn("swap: fallocate failed, continuing without swap", "err", err)
		_ = os.Remove(path)
		return nil
	}
	// mkswap refuses a world-readable file, and its contents are process memory.
	if err := os.Chmod(path, 0o600); err != nil {
		slog.Warn("swap: chmod failed, continuing without swap", "err", err)
		_ = os.Remove(path)
		return nil
	}
	if err := run("mkswap", path); err != nil {
		slog.Warn("swap: mkswap failed, continuing without swap", "err", err)
		_ = os.Remove(path)
		return nil
	}
	slog.Info("swap: created", "path", "/"+swapFileName, "bytes", size)
	return nil
}

// swapSize returns the swap file size for the filesystem mounted at root, or 0
// when the disk cannot spare a useful amount. SPINIFEX_SWAP_SIZE overrides the
// calculation; 0 disables swap entirely.
//
// RAM drives the size and the disk only caps it, so a small test VM stays cheap
// on disk while a real host gets enough to carry a large import.
func swapSize(root string) (int64, error) {
	if v := strings.TrimSpace(os.Getenv("SPINIFEX_SWAP_SIZE")); v != "" {
		size, err := parseSize(v)
		if err != nil {
			return 0, fmt.Errorf("SPINIFEX_SWAP_SIZE: %w", err)
		}
		return size, nil
	}

	ram, err := totalRAM()
	if err != nil {
		return 0, err
	}
	size := ram / 2
	if ram >= swapRAMThreshold {
		size = swapLargeBytes
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", root, err)
	}
	// Free space, not total: the rootfs is already copied in, so this is what is
	// genuinely left to give away.
	free := int64(st.Bavail) * st.Bsize
	if diskCap := free / swapDiskShare; size > diskCap {
		size = diskCap
	}
	if size < swapMinBytes {
		return 0, nil
	}
	return size, nil
}

// totalRAM reports installed memory in bytes.
func totalRAM() (int64, error) {
	var si syscall.Sysinfo_t
	if err := syscall.Sysinfo(&si); err != nil {
		return 0, fmt.Errorf("sysinfo: %w", err)
	}
	unit := int64(si.Unit)
	if unit == 0 {
		unit = 1
	}
	return int64(si.Totalram) * unit, nil
}

// parseSize reads a plain byte count or a G/M suffixed size ("32G", "512M").
func parseSize(s string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "G"), strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative size %q", s)
	}
	return n * mult, nil
}

func partUUID(dev string) (string, error) {
	out, err := exec.Command("blkid", "-s", "UUID", "-o", "value", dev).Output()
	if err != nil {
		return "", fmt.Errorf("blkid %s: %w", dev, err)
	}
	uuid := strings.TrimSpace(string(out))
	if uuid == "" {
		return "", fmt.Errorf("blkid returned no UUID for %s — partition may not have a filesystem yet", dev)
	}
	return uuid, nil
}

// copyGrubFont copies the unicode.pf2 GRUB font into the installed system's
// /boot/grub/fonts/ directory. This ensures update-grub finds the font at
// /boot/grub/fonts/unicode.pf2 — the same path the ISO's grub.cfg uses —
// so the generated grub.cfg enables gfxterm and the background image.
// Without this, grub-mkconfig falls back to /usr/share/grub/unicode.pf2,
// a path that GRUB 2.12 (trixie) may fail to resolve at boot time.
// Non-fatal — a missing source is logged and skipped.
func copyGrubFont(root string) {
	candidates := []string{
		"/cdrom/boot/grub/fonts/unicode.pf2", // ISO tree (preferred)
		"/usr/share/grub/unicode.pf2",        // live system grub-common fallback
	}
	for _, src := range candidates {
		in, err := os.Open(src)
		if err != nil {
			continue
		}
		defer in.Close()
		dstDir := filepath.Join(root, "boot/grub/fonts")
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			slog.Warn("copyGrubFont: cannot create fonts dir", "err", err)
			return
		}
		out, err := os.OpenFile(filepath.Join(dstDir, "unicode.pf2"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			slog.Warn("copyGrubFont: cannot open destination", "err", err)
			return
		}
		defer out.Close()
		if _, err := io.Copy(out, in); err != nil {
			slog.Warn("copyGrubFont: copy failed", "err", err)
		}
		return
	}
	slog.Warn("copyGrubFont: no unicode.pf2 found, splash may not display")
}

// copySplashImage copies the GRUB splash from the live ISO (/cdrom/boot/grub/splash.png)
// into the installed system so the post-install GRUB shows the same branded background
// as the installer. Non-fatal — a missing or unreadable source is logged and skipped.
func copySplashImage(root string) {
	const src = "/usr/share/spinifex/grub-splash.png"
	in, err := os.Open(src)
	if err != nil {
		slog.Warn("copySplashImage: splash not found, skipping", "path", src)
		return
	}
	defer in.Close()

	dstDir := filepath.Join(root, "boot/grub")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		slog.Warn("copySplashImage: cannot create boot/grub dir", "err", err)
		return
	}
	out, err := os.OpenFile(filepath.Join(dstDir, "splash.png"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		slog.Warn("copySplashImage: cannot open destination", "err", err)
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		slog.Warn("copySplashImage: copy failed", "err", err)
	}
}

// maskSystemdUnit creates a symlink to /dev/null for the given unit, which is
// the standard way to permanently disable a unit so systemd will never start it.
func maskSystemdUnit(root, unit string) error {
	unitPath := filepath.Join(root, "etc/systemd/system", unit)
	_ = os.Remove(unitPath)
	return os.Symlink("/dev/null", unitPath)
}

// setShadowPassword sets a Unix password on the installed system without
// going through PAM. See the long comment in installSpinifex for the
// rationale.
func setShadowPassword(user, password string) error {
	cmd := exec.Command("chpasswd", "-c", "YESCRYPT", "-R", mountRoot)
	cmd.Stdin = strings.NewReader(user + ":" + password)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// chrootMountPaths lists virtual filesystems to bind-mount into the chroot.
// Order matters: unbind in reverse.
var chrootMountPaths = []string{"dev", "proc", "sys"}

// bindChrootMounts bind-mounts /dev, /proc, and /sys into the installed rootfs
// so chroot commands (chpasswd, systemd-machine-id-setup, update-grub) can
// access hardware, process info, and entropy sources. Idempotent — already-
// mounted paths are skipped.
func bindChrootMounts() error {
	for _, m := range chrootMountPaths {
		dst := filepath.Join(mountRoot, m)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("create chroot mountpoint /%s: %w", m, err)
		}
		if err := run("mount", "--bind", "/"+m, dst); err != nil {
			return fmt.Errorf("bind-mount /%s into chroot: %w", m, err)
		}
	}
	return nil
}

// unbindChrootMounts unmounts the virtual filesystems in reverse order.
// Errors are logged but not returned — this is best-effort cleanup.
func unbindChrootMounts() {
	for _, v := range slices.Backward(chrootMountPaths) {
		// Quiet: this also runs from the deferred cleanup, where nothing is
		// mounted and three "no mount point specified" lines above a real error
		// message are pure distraction.
		_ = runQuiet("umount", filepath.Join(mountRoot, v))
	}
}

var run = func(name string, args ...string) error {
	return runEnv(nil, name, args...)
}

// runQuiet is run with output discarded, for best-effort probes whose failure
// is expected and whose stderr would otherwise look like an install fault.
var runQuiet = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

// runEnv is run with extra environment variables appended to the installer's own.
var runEnv = func(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.Run()
}
