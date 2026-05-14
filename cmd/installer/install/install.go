/*
Copyright © 2026 Mulga Defense Corporation

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

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
	"strings"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/cmd/installer/firstboot"
	"github.com/mulgadc/spinifex/cmd/installer/systemd"
)

const (
	mountRoot = "/mnt/spinifex-install"
	efiPart   = mountRoot + "/boot/efi"
)

// Run executes all installation steps in order. It is intentionally sequential
// and explicit — each step is visible in logs so failures are easy to diagnose.
func Run(cfg *Config) error {
	// The live environment may not have /sbin or /usr/sbin in PATH. Set it
	// explicitly so exec.Command's LookPath finds system binaries like grub-install.
	_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

	// Unmount unconditionally on exit so a failed step never leaves partitions
	// mounted in the live environment, which would cause a retry to double-mount.
	defer func() {
		_ = run("umount", efiPart)
		_ = run("umount", mountRoot)
	}()

	steps := []struct {
		name string
		fn   func() error
	}{
		{"partition disk", func() error { return partitionDisk(cfg.Disk) }},
		{"format partitions", func() error { return formatPartitions(cfg.Disk) }},
		{"mount partitions", func() error { return mountPartitions(cfg.Disk) }},
		{"copy rootfs", copyRootfs},
		{"write fstab", func() error { return writeFstab(cfg.Disk) }},
		{"install spinifex", func() error { return installSpinifex(cfg) }},
		{"write network config", func() error { return writeNetworkConfig(cfg) }},
		{"write firstboot service", func() error { return firstboot.Write(mountRoot, cfg.toFirstbootConfig()) }},
		{"install bootloader", func() error { return installBootloader(cfg.Disk) }},
		{"install CA cert", func() error { return installCACert(cfg) }},
	}

	for _, s := range steps {
		slog.Info("installer", "step", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("step %q: %w", s.name, err)
		}
	}

	slog.Info("installation complete")
	fireInstallCallback()
	promptRemoveUSB()
	return reboot()
}

func partitionDisk(disk string) error {
	// GPT table with three partitions:
	//   p1: 1MiB BIOS Boot Partition — required for grub-install i386-pc on GPT
	//   p2: 512MiB EFI System Partition
	//   p3: remainder as root (ext4)
	if err := run("parted", "--script", disk,
		"mklabel", "gpt",
		"mkpart", "bios_boot", "1MiB", "2MiB",
		"set", "1", "bios_grub", "on",
		"mkpart", "ESP", "fat32", "2MiB", "514MiB",
		"set", "2", "esp", "on",
		"mkpart", "root", "ext4", "514MiB", "100%",
	); err != nil {
		return err
	}
	// Force the kernel to re-read the partition table and wait for udev to
	// create the partition device nodes. Without this, mkfs.fat in the next
	// step may race and fail with "No such file or directory" on /dev/sda2 —
	// the kernel has accepted the new layout but /dev hasn't been populated
	// yet. Trixie's udev seems slower at this than Bookworm's was.
	return waitForPartitions(disk)
}

// waitForPartitions ensures the EFI and root partition device nodes exist
// after parted creates them. It runs partprobe (kernel re-read) and
// udevadm settle (wait for queued events), then polls /dev for the files.
func waitForPartitions(disk string) error {
	// Best-effort: partprobe failure isn't fatal — udev may still pick up
	// the change from the BLKRRPART ioctl that parted itself issued.
	if err := run("partprobe", disk); err != nil {
		slog.Warn("partprobe failed, continuing", "disk", disk, "err", err)
	}
	if err := run("udevadm", "settle", "--timeout=10"); err != nil {
		slog.Warn("udevadm settle failed, continuing", "err", err)
	}
	efi, root := partitionPaths(disk)
	deadline := time.Now().Add(15 * time.Second)
	for _, part := range []string{efi, root} {
		for {
			if _, err := os.Stat(part); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("partition device %s did not appear within timeout — kernel/udev did not pick up new partition table", part)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil
}

func formatPartitions(disk string) error {
	efi, root := partitionPaths(disk)
	if err := run("mkfs.fat", "-F32", efi); err != nil {
		return err
	}
	return run("mkfs.ext4", "-F", root)
}

func mountPartitions(disk string) error {
	_, root := partitionPaths(disk)
	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		return err
	}
	if err := run("mount", root, mountRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(efiPart, 0o755); err != nil {
		return err
	}
	efi, _ := partitionPaths(disk)
	return run("mount", efi, efiPart)
}

// copyRootfs copies the live squashfs environment onto the target disk using
// rsync. This is the air-gapped alternative to debootstrap — all packages are
// already embedded in the ISO so no network access is required.
func copyRootfs() error {
	if err := run("rsync", "-aHAX", "--delete", "--info=progress2",
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
		"/", mountRoot+"/",
	); err != nil {
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
		cfg.WANAddress, cfg.Hostname)
	confDir := filepath.Join(mountRoot, "etc/spinifex")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(confDir, "node.conf"), []byte(nodeConf), 0o644); err != nil {
		return err
	}

	// dhcpcd-base is present on the installed system (used by setup-ovn.sh for
	// macvlan mode). Mask the standalone dhcpcd.service so it never auto-starts
	// and races with systemd-networkd's built-in DHCP client on br-wan.
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

	if err := writeNetworkdBridge(netdDir, cfg.WANInterface, "br-wan", false,
		cfg.WANDHCPMode, cfg.WANAddress, cfg.WANMask, cfg.WANGateway, cfg.WANDNS,
		cfg.WANWiFiSSID, cfg.WANWiFiPass); err != nil {
		return err
	}

	if cfg.LANInterface != "" {
		if err := writeNetworkdBridge(netdDir, cfg.LANInterface, "br-lan", true,
			cfg.LANDHCPMode, cfg.LANAddress, cfg.LANMask, "", cfg.LANDNS,
			cfg.LANWiFiSSID, cfg.LANWiFiPass); err != nil {
			return err
		}
		// br-lan timing is controlled by spinifex-lan-bridge.service, not
		// networkd auto-activation — ActivationPolicy=manual in the .network
		// file ensures networkd creates the bridge but does not bring it up.
		if err := systemd.WriteLANBridgeUnit(mountRoot); err != nil {
			return fmt.Errorf("lan bridge unit: %w", err)
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
	bridges := []string{"br-wan"}
	if cfg.LANInterface != "" {
		bridges = append(bridges, "br-lan")
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
	for _, iface := range []string{cfg.WANInterface, cfg.LANInterface} {
		if iface == "" {
			continue
		}
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

// writeNetworkdBridge writes the three systemd-networkd files that configure a
// physical NIC enslaved to a Linux bridge:
//
//   - {prio}-spinifex-{suffix}-nic.network  enslaves the NIC to the bridge
//   - {prio+1}-spinifex-{suffix}.netdev     declares the bridge device
//   - {prio+1}-spinifex-{suffix}.network    configures IP on the bridge
//
// manual=true sets ActivationPolicy=manual so networkd creates the bridge but
// does not auto-activate it — used for br-lan, which spinifex-lan-bridge.service
// activates after network-online.target.
func writeNetworkdBridge(dir, nicIface, bridgeName string, manual, dhcp bool,
	addr, mask, gw string, dns []string, wifiSSID, wifiPass string) error {
	suffix := strings.TrimPrefix(bridgeName, "br-")
	nicPrio, brPrio := 10, 11
	if manual {
		nicPrio, brPrio = 12, 13
	}

	// NIC .network — enslaves the physical NIC to the bridge.
	nicNet := fmt.Sprintf("[Match]\nName=%s\n\n[Network]\nBridge=%s\n", nicIface, bridgeName)
	nicFile := fmt.Sprintf("%02d-spinifex-%s-nic.network", nicPrio, suffix)
	if err := os.WriteFile(filepath.Join(dir, nicFile), []byte(nicNet), 0o644); err != nil {
		return err
	}

	// Bridge .netdev — declares the bridge device.
	brNetdev := fmt.Sprintf("[NetDev]\nName=%s\nKind=bridge\n\n[Bridge]\nSTP=no\n", bridgeName)
	brNetdevFile := fmt.Sprintf("%02d-spinifex-%s.netdev", brPrio, suffix)
	if err := os.WriteFile(filepath.Join(dir, brNetdevFile), []byte(brNetdev), 0o644); err != nil {
		return err
	}

	// Bridge .network — IP configuration.
	var b strings.Builder
	fmt.Fprintf(&b, "[Match]\nName=%s\n\n", bridgeName)
	if manual {
		b.WriteString("[Link]\nActivationPolicy=manual\nRequiredForOnline=no\n\n")
	}
	b.WriteString("[Network]\n")
	if dhcp {
		b.WriteString("DHCP=ipv4\n")
	} else {
		cidr, err := addrCIDR(addr, mask)
		if err != nil {
			return fmt.Errorf("bridge %s: %w", bridgeName, err)
		}
		fmt.Fprintf(&b, "Address=%s\n", cidr)
		if gw != "" {
			fmt.Fprintf(&b, "Gateway=%s\n", gw)
		}
		for _, ns := range dns {
			if ns = strings.TrimSpace(ns); ns != "" {
				fmt.Fprintf(&b, "DNS=%s\n", ns)
			}
		}
	}
	b.WriteString("IPv6AcceptRA=no\nConfigureWithoutCarrier=yes\n")
	if manual && dhcp {
		// br-lan is non-critical; fail fast on DHCP so a missing LAN cable
		// does not stall spinifex-lan-bridge.service indefinitely.
		b.WriteString("\n[DHCP]\nTimeout=10\n")
	}
	brNetFile := fmt.Sprintf("%02d-spinifex-%s.network", brPrio, suffix)
	if err := os.WriteFile(filepath.Join(dir, brNetFile), []byte(b.String()), 0o644); err != nil {
		return err
	}

	if wifiSSID != "" {
		if err := writeWPASupplicant(nicIface, wifiSSID, wifiPass); err != nil {
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

func installBootloader(disk string) error {
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
		disk,
	); biosErr != nil {
		if efiErr != nil {
			// Both targets failed — the system will not boot.
			return fmt.Errorf("both bootloader targets failed (EFI: %v; BIOS: %w)", efiErr, biosErr)
		}
		return biosErr
	}
	// Copy splash image and unicode font from the ISO (mounted at /cdrom) so the
	// installed GRUB shows the same branded background as the installer GRUB.
	// The font must be at /boot/grub/fonts/unicode.pf2 so update-grub finds it
	// there and emits the same loadfont path as the ISO's grub.cfg — GRUB 2.12
	// (trixie) needs the font in the boot partition, not just /usr/share/grub/.
	copySplashImage(mountRoot)
	copyGrubFont(mountRoot)

	// Both consoles listed: tty0 stays primary for local install, ttyS0 mirrors
	// kernel output to serial so headless installs (CI, racks with serial-only
	// IPMI, remote-dev SSH-to-QEMU) see boot messages. The last `console=` on
	// the cmdline becomes the system console, so ttyS0 wins on serial-only
	// hardware while tty0 still receives output for local display.
	grubDefault := `GRUB_DEFAULT=0
GRUB_TIMEOUT=5
GRUB_DISTRIBUTOR=Spinifex
GRUB_CMDLINE_LINUX_DEFAULT=""
GRUB_CMDLINE_LINUX="console=tty0 console=ttyS0,115200 systemd.show_status=1"
`

	if err := os.WriteFile(filepath.Join(mountRoot, "etc/default/grub"), []byte(grubDefault), 0o644); err != nil {
		return fmt.Errorf("write /etc/default/grub: %w", err)
	}

	// Mirror the ISO grub.cfg graphical block exactly so the installed GRUB menu
	// looks identical to the installer menu. gfxterm MUST be activated before
	// serial is appended — background_image silently does nothing in text mode.
	// Using exec tail so update-grub includes everything from line 3 as raw GRUB config.
	grubTheme := `#!/bin/sh
exec tail -n +3 $0
insmod all_video
insmod font
if loadfont /boot/grub/fonts/unicode.pf2; then
  set gfxmode=auto
  insmod gfxterm
  terminal_output gfxterm
fi
insmod serial
if serial --unit=0 --speed=115200 --timeout=1; then
  terminal_input  --append serial
  terminal_output --append serial
fi

# --- Branding ---
insmod png
set theme=/boot/grub/theme.txt
export theme
`
	if err := os.WriteFile(filepath.Join(mountRoot, "etc/grub.d/06_spinifex"), []byte(grubTheme), 0o755); err != nil {
		return fmt.Errorf("write /etc/grub.d/06_spinifex: %w", err)
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
	// Geneve encap IP: use LAN bridge IP when a dedicated LAN NIC is present,
	// otherwise fall back to WAN bridge IP. Empty for DHCP — setup-ovn.sh
	// auto-detects the IP from the default route at boot in that case.
	encapIP := c.WANAddress
	if c.LANInterface != "" && c.LANAddress != "" {
		encapIP = c.LANAddress
	}
	return firstboot.Config{
		Hostname:        c.Hostname,
		EncapIP:         encapIP,
		ClusterRole:     c.ClusterRole,
		JoinAddr:        c.JoinAddr,
		Email:           c.Email,
		InstallCallback: strings.TrimSpace(os.Getenv("SPINIFEX_INSTALL_CALLBACK")),
	}
}

// writeFstab writes /etc/fstab on the installed system using partition UUIDs so
// the root filesystem is mounted read-write at boot and the EFI partition is
// mounted at /boot/efi.
func writeFstab(disk string) error {
	efi, root := partitionPaths(disk)
	rootUUID, err := partUUID(root)
	if err != nil {
		return fmt.Errorf("get root UUID: %w", err)
	}
	efiUUID, err := partUUID(efi)
	if err != nil {
		return fmt.Errorf("get EFI UUID: %w", err)
	}
	fstab := fmt.Sprintf("# /etc/fstab — generated by Spinifex installer\nUUID=%s / ext4 errors=remount-ro 0 1\nUUID=%s /boot/efi vfat umask=0077 0 1\n",
		rootUUID, efiUUID)
	return os.WriteFile(filepath.Join(mountRoot, "etc/fstab"), []byte(fstab), 0o644)
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

// partitionPaths returns the EFI and root partition device paths for a given
// disk. p1 is the BIOS Boot Partition (no filesystem), p2 is EFI, p3 is root.
// Handles both /dev/sdX (→ /dev/sdX2, /dev/sdX3) and /dev/nvmeXnY
// (→ /dev/nvmeXnYp2, /dev/nvmeXnYp3).
func partitionPaths(disk string) (efi, root string) {
	// NVMe devices use a 'p' separator before the partition number.
	if len(disk) > 0 && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return disk + "p2", disk + "p3"
	}
	return disk + "2", disk + "3"
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
	for i := len(chrootMountPaths) - 1; i >= 0; i-- {
		_ = run("umount", filepath.Join(mountRoot, chrootMountPaths[i]))
	}
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
