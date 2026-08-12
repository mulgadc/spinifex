package install

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// partitionSettleTimeout bounds the wait for the kernel and udev to publish a
// disk's new partition table. A var so tests do not pay it.
var partitionSettleTimeout = 15 * time.Second

// releaseDisk detaches whatever the live environment has attached to a disk
// before it is erased. A mounted filesystem or an active swap area makes
// BLKRRPART return EBUSY, and the kernel then keeps serving the disk's old
// partition table while sgdisk writes the new one and reports success.
func releaseDisk(d Disk) {
	for _, m := range mountsOnDisk(d.Path) {
		err := runQuiet("umount", m.Device)
		if err != nil {
			// Lazy detach: a busy mount must not stall an install that is about to
			// erase the filesystem underneath it anyway.
			err = runQuiet("umount", "-l", m.Device)
		}
		if err != nil {
			slog.Warn("could not unmount before erase", "device", m.Device, "mountpoint", m.Mountpoint, "err", err)
			continue
		}
		slog.Info("released mount before erase", "device", m.Device, "mountpoint", m.Mountpoint)
	}
	for _, s := range swapsOnDisk(d.Path) {
		if err := runQuiet("swapoff", s); err != nil {
			slog.Warn("could not disable swap before erase", "device", s, "err", err)
			continue
		}
		slog.Info("disabled swap before erase", "device", s)
	}
}

// settlePartitions forces the kernel to re-read a disk's partition table and
// blocks until its view matches want, naming the partitions by number.
//
// An empty want asserts the disk has no partitions at all. That is the only
// state no leftover can satisfy: a disk whose previous table had the same
// partition numbers as the one being written — an old OS install is p1, p2, p3,
// exactly what the boot layout writes — passes an "are the expected partitions
// there" check on stale nodes alone.
func settlePartitions(d Disk, want ...int) error {
	if err := runQuiet("partprobe", d.Path); err != nil {
		// partprobe rescans the whole disk and fails if it cannot open any part of
		// it; the ioctl on its own sometimes still gets through.
		if err := runQuiet("blockdev", "--rereadpt", d.Path); err != nil {
			slog.Warn("could not force a partition table re-read", "disk", d.Path, "err", err)
		}
	}
	_ = runQuiet("udevadm", "settle", "--timeout=10")

	wanted := make([]string, 0, len(want))
	for _, n := range want {
		wanted = append(wanted, filepath.Base(d.PartitionPath(n)))
	}
	slices.Sort(wanted)

	deadline := time.Now().Add(partitionSettleTimeout)
	for {
		got, err := kernelPartitions(d.Path)
		if err != nil {
			return err
		}
		if slices.Equal(got, wanted) {
			return nil
		}
		if time.Now().After(deadline) {
			return staleTableError(d, wanted, got)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// staleTableError explains a kernel partition view that never caught up. The
// erase case gets its own wording because it is the dangerous one — proceeding
// would format the previous layout — and because the holder is usually the
// whole diagnosis.
func staleTableError(d Disk, wanted, got []string) error {
	if len(wanted) == 0 {
		msg := fmt.Sprintf("%s still shows partitions [%s] after its table was erased — the kernel is using the old table, so formatting would write at the previous layout's offsets",
			d.Path, strings.Join(got, ", "))
		if h := diskHolders(d.Path); len(h) > 0 {
			return fmt.Errorf("%s; it is held by %s, which must be stopped before this disk can be installed to",
				msg, strings.Join(h, ", "))
		}
		return fmt.Errorf("%s; something in the live environment is holding it open", msg)
	}
	return fmt.Errorf("%s: the kernel shows partitions [%s] but the new table has [%s] — the table was written but never picked up",
		d.Path, strings.Join(got, ", "), strings.Join(wanted, ", "))
}

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
	return settlePartitions(rm.Disk, dataPartNum)
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
	return settlePartitions(d, biosPartNum, espPartNum, rootPartNum)
}

// waitForPartitions ensures every partition device node the installer is about
// to use exists. The kernel's own view was already asserted per disk as each
// table was written; udev publishes the /dev nodes and by-id symlinks after
// that, and Trixie is slow enough about it that mkfs races them.
func waitForPartitions(cfg DiskConfig) error {
	deadline := time.Now().Add(partitionSettleTimeout)
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
