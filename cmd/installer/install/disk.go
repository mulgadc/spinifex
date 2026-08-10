package install

import (
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Partition numbers, identical on every disk the installer touches so that a
// replacement drive can be cloned with `sgdisk -R` and nothing has to be
// recomputed at 3am.
const (
	biosPartNum = 1
	espPartNum  = 2
	rootPartNum = 3

	// dataPartNum is the sole partition on a role drive. Data drives carry no
	// bootloader, so they start at 1 rather than mirroring the boot layout.
	dataPartNum = 1
)

// GPT partition type GUIDs, by their sgdisk short code.
const (
	typeBIOSBoot = "EF02"
	typeESP      = "EF00"
	typeLinuxFS  = "8300"
	typeZFS      = "BF00"
)

// Partition geometry. The ESP is sized as Proxmox does: 512MiB is enough for
// two kernels, but a 1GiB ESP on a real disk costs nothing and keeps room for
// the extra kernels an operator pins during an upgrade.
const (
	espSmallMiB     = 512
	espLargeMiB     = 1024
	espLargeDiskGiB = 100

	// biosBootMiB is the BIOS Boot Partition GRUB's i386-pc core.img is embedded
	// into. GPT leaves no post-MBR gap, so without it there is nowhere to put it.
	biosBootMiB = 1

	// tailReserveMiB is left unallocated at the end of every disk. A replacement
	// drive of the "same" size is routinely a few thousand sectors smaller, and
	// without slack `sgdisk -R` refuses to clone the table onto it — which is
	// discovered while standing in front of a degraded pool.
	tailReserveMiB = 1024
)

// espSizeMiB picks the ESP size for a disk.
func espSizeMiB(d Disk) int {
	if d.Bytes >= espLargeDiskGiB<<30 {
		return espLargeMiB
	}
	return espSmallMiB
}

// partitionDisks lays down an identical GPT on every selected disk. Each disk
// gets a BIOS boot partition and an ESP whether or not this machine booted in
// EFI mode: the cost is 1GiB, and it means a board swap or a firmware change
// from BIOS to UEFI does not leave an unbootable pool.
func partitionDisks(cfg DiskConfig) error {
	ptype := typeLinuxFS
	if cfg.FS.IsZFS() {
		ptype = typeZFS
	}
	for _, d := range cfg.bootDisks() {
		if err := partitionOne(d, ptype); err != nil {
			return fmt.Errorf("partition %s: %w", d.Path, err)
		}
	}
	for _, rm := range cfg.DataMounts() {
		if err := partitionDataDisk(rm); err != nil {
			return fmt.Errorf("partition %s (%s): %w", rm.Disk.Path, rm.Role, err)
		}
	}
	return waitForPartitions(cfg)
}

// partitionDataDisk writes the single-partition GPT for a role drive. It gets
// no bios_boot and no ESP: nothing installs a bootloader there, and a formatted
// ESP no firmware entry points at only misleads whoever reads lsblk in an
// incident.
func partitionDataDisk(rm RoleMount) error {
	if err := clearDisk(rm.Disk); err != nil {
		return fmt.Errorf("clear: %w", err)
	}
	// Same tail reserve as a boot drive, for the same reason: a replacement of
	// the "same" size is routinely a few thousand sectors smaller.
	if err := run("sgdisk",
		"-n", fmt.Sprintf("%d:1M:-%dM", dataPartNum, tailReserveMiB),
		"-t", fmt.Sprintf("%d:%s", dataPartNum, typeLinuxFS),
		"-c", fmt.Sprintf("%d:%s", dataPartNum, rm.Role.Label()),
		rm.Disk.Path,
	); err != nil {
		return err
	}
	if err := run("sgdisk", "-G", rm.Disk.Path); err != nil {
		slog.Warn("could not randomise GPT GUIDs", "disk", rm.Disk.Path, "err", err)
	}
	return nil
}

// partitionOne writes the GPT for a single disk.
func partitionOne(d Disk, rootType string) error {
	// Stale ZFS labels and filesystem signatures survive a new partition table
	// and make a retried install fail with errors that read like hardware faults.
	if err := clearDisk(d); err != nil {
		return fmt.Errorf("clear: %w", err)
	}

	esp := espSizeMiB(d)
	if err := run("sgdisk",
		"-n", fmt.Sprintf("%d:1M:+%dM", biosPartNum, biosBootMiB),
		"-t", fmt.Sprintf("%d:%s", biosPartNum, typeBIOSBoot),
		"-c", fmt.Sprintf("%d:bios_boot", biosPartNum),
		"-n", fmt.Sprintf("%d:0:+%dM", espPartNum, esp),
		"-t", fmt.Sprintf("%d:%s", espPartNum, typeESP),
		"-c", fmt.Sprintf("%d:ESP", espPartNum),
		"-n", fmt.Sprintf("%d:0:-%dM", rootPartNum, tailReserveMiB),
		"-t", fmt.Sprintf("%d:%s", rootPartNum, rootType),
		"-c", fmt.Sprintf("%d:root", rootPartNum),
		d.Path,
	); err != nil {
		return err
	}
	// A fresh disk-level GUID: cloning a table with `sgdisk -R` copies it, and
	// two disks sharing one leaves the pool ambiguous to udev.
	if err := run("sgdisk", "-G", d.Path); err != nil {
		slog.Warn("could not randomise GPT GUIDs", "disk", d.Path, "err", err)
	}
	return nil
}

// waitForPartitions ensures every partition device node the installer is about
// to use exists. Trixie's udev is slow enough after a table rewrite that mkfs
// races it and fails with ENOENT on a device the kernel has already accepted.
func waitForPartitions(cfg DiskConfig) error {
	for _, d := range cfg.Disks {
		// Best-effort: partprobe failing is not fatal, since udev may still act
		// on the BLKRRPART ioctl sgdisk issued itself.
		if err := run("partprobe", d.Path); err != nil {
			slog.Warn("partprobe failed, continuing", "disk", d.Path, "err", err)
		}
	}
	if err := run("udevadm", "settle", "--timeout=10"); err != nil {
		slog.Warn("udevadm settle failed, continuing", "err", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for _, d := range cfg.bootDisks() {
		wanted := []string{d.PartitionPath(espPartNum), d.PartitionPath(rootPartNum)}
		// The pool is built from by-id paths, so their symlinks have to exist
		// too — udev publishes them later than the kernel device node.
		if d.ByID != "" {
			wanted = append(wanted, d.StablePartitionPath(rootPartNum))
		}
		for _, part := range wanted {
			if err := waitForPath(part, deadline); err != nil {
				return err
			}
		}
	}
	for _, rm := range cfg.DataMounts() {
		if err := waitForPath(rm.Disk.PartitionPath(dataPartNum), deadline); err != nil {
			return err
		}
	}
	return nil
}

func waitForPath(path string, deadline time.Time) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device %s did not appear within timeout — the kernel or udev did not pick up the new partition table", path)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// formatESPs makes a FAT32 filesystem on every disk's ESP. Each one is
// independent — there is no RAID here, they are kept in step by content.
func formatESPs(cfg DiskConfig) error {
	for _, d := range cfg.bootDisks() {
		esp := d.PartitionPath(espPartNum)
		// A distinct label per member so `lsblk -f` on a degraded machine tells
		// the operator which disk each ESP belongs to.
		if err := run("mkfs.fat", "-F", "32", "-n", "EFI", esp); err != nil {
			return fmt.Errorf("format ESP %s: %w", esp, err)
		}
	}
	return nil
}
