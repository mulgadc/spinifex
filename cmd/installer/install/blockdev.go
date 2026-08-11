package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Disk is one candidate block device with every attribute the installer needs
// to validate a selection: sizes for the same-size rule, block sizes for ashift
// and the 4Kn/BIOS check, rotational for autotrim, and ByID for pool members.
type Disk struct {
	Path  string // /dev/sda
	ByID  string // /dev/disk/by-id/ata-… — stable across controller reordering
	Bytes int64
	Model string

	LogicalBlockSize  int
	PhysicalBlockSize int

	Rotational bool
	Removable  bool

	// LiveMedia marks the device the installer itself booted from. Selecting it
	// would erase the running system mid-install.
	LiveMedia bool

	// Content is a human-readable summary of what is on the disk today
	// ("ext4 filesystem", "empty"), shown before the operator confirms erasure.
	Content string
}

// skipPrefixes are kernel block devices that are never install targets:
// virtual, mapped, or removable media. Mirrors Proxmox's hd_list filter.
var skipPrefixes = []string{"ram", "loop", "md", "dm-", "fd", "sr", "zram", "nbd"}

// The kernel-owned trees the scan reads, indirected so tests can point it at a
// fixture instead of the running machine's hardware.
var (
	sysBlockDir      = "/sys/block"
	sysClassBlockDir = "/sys/class/block"
	devByIDDir       = "/dev/disk/by-id"
	procMountsPath   = "/proc/mounts"
	procSwapsPath    = "/proc/swaps"
)

// ListDisks enumerates install-candidate disks from /sys/block. It reads sysfs
// and blkid only — pool detection is separate (ImportablePools) because it is
// far slower and not every caller needs it.
func ListDisks() ([]Disk, error) {
	entries, err := os.ReadDir(sysBlockDir)
	if err != nil {
		return nil, fmt.Errorf("read /sys/block: %w", err)
	}

	byID := buildByIDIndex()
	live := liveMediaDevices()

	var disks []Disk
	for _, e := range entries {
		name := e.Name()
		if hasAnyPrefix(name, skipPrefixes) {
			continue
		}
		// A zero-sized device is an empty card reader slot, not a disk.
		sectors := sysfsInt(name, "size")
		if sectors == 0 {
			continue
		}
		d := Disk{
			Path:              "/dev/" + name,
			ByID:              byID[name],
			Bytes:             sectors * 512,
			Model:             sysfsString(name, "device/model"),
			LogicalBlockSize:  sysfsIntDefault(name, "queue/logical_block_size", 512),
			PhysicalBlockSize: sysfsIntDefault(name, "queue/physical_block_size", 512),
			Rotational:        sysfsInt(name, "queue/rotational") == 1,
			Removable:         sysfsInt(name, "removable") == 1,
			LiveMedia:         live[name],
		}
		d.Content = describeContent(d.Path)
		disks = append(disks, d)
	}
	slices.SortFunc(disks, func(a, b Disk) int { return strings.Compare(a.Path, b.Path) })
	return disks, nil
}

// SizeHuman renders the disk size for display, e.g. "1.7T".
func (d Disk) SizeHuman() string {
	switch {
	case d.Bytes >= 1<<40:
		return fmt.Sprintf("%.1fT", float64(d.Bytes)/(1<<40))
	case d.Bytes >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(d.Bytes)/(1<<30))
	case d.Bytes >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(d.Bytes)/(1<<20))
	default:
		return fmt.Sprintf("%dB", d.Bytes)
	}
}

// Stable returns the path to use when building a pool: the by-id path when one
// exists, else the kernel name. Pools referencing /dev/sdX break when a
// controller enumerates disks in a different order after a reboot.
func (d Disk) Stable() string {
	if d.ByID != "" {
		return d.ByID
	}
	return d.Path
}

// PartitionPath returns the device path for partition n of this disk. NVMe and
// other devices whose name ends in a digit take a 'p' separator.
func (d Disk) PartitionPath(n int) string { return partitionPath(d.Path, n) }

// StablePartitionPath is PartitionPath against the by-id path, which udev
// publishes with a "-partN" suffix rather than the kernel's 'p' convention.
func (d Disk) StablePartitionPath(n int) string {
	if d.ByID == "" {
		return d.PartitionPath(n)
	}
	return fmt.Sprintf("%s-part%d", d.ByID, n)
}

func partitionPath(disk string, n int) string {
	if len(disk) > 0 && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

// readKernelPartitions returns the partition device names the kernel currently
// publishes for a disk, sorted.
//
// It reads sysfs rather than /dev deliberately. A partition device node
// outlives the table that created it, so a stale node is indistinguishable
// from a fresh one by existence alone — which is how a disk whose old table
// the kernel never dropped passes every check and then gets formatted at the
// previous layout's offsets.
func readKernelPartitions(disk string) ([]string, error) {
	name := filepath.Base(disk)
	entries, err := os.ReadDir(filepath.Join(sysBlockDir, name))
	if err != nil {
		return nil, fmt.Errorf("read the kernel's partition list for %s: %w", disk, err)
	}
	var out []string
	for _, e := range entries {
		// Partitions are the child directories carrying a "partition" attribute.
		// "queue", "device" and the plain attribute files sit alongside them.
		if _, err := os.Stat(filepath.Join(sysBlockDir, name, e.Name(), "partition")); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out, nil
}

// kernelPartitions is indirected so tests can drive the partition-table
// assertions without a real block device.
var kernelPartitions = readKernelPartitions

// partitionDevice maps a kernel partition name back to its device node,
// resolved alongside the disk it belongs to rather than assuming /dev.
func partitionDevice(disk, part string) string {
	return filepath.Join(filepath.Dir(disk), part)
}

// diskHolders names the md, LVM or device-mapper devices claiming this disk or
// one of its partitions. A holder is the usual reason the kernel refuses to
// re-read a partition table, so it is what the operator needs told when the
// installer gives up on a disk.
func diskHolders(disk string) []string {
	name := filepath.Base(disk)
	dirs := []string{filepath.Join(sysBlockDir, name, "holders")}
	parts, _ := kernelPartitions(disk)
	for _, p := range parts {
		dirs = append(dirs, filepath.Join(sysBlockDir, name, p, "holders"))
	}

	seen := map[string]bool{}
	var out []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if seen[e.Name()] {
				continue
			}
			seen[e.Name()] = true
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// diskMount is one filesystem mounted from a disk the installer is about to
// erase.
type diskMount struct {
	Device     string
	Mountpoint string
}

// devicesOnDisk is the set of device paths belonging to a disk: the whole disk
// and every partition the kernel currently publishes. Matching /proc tables
// against a set rather than a string prefix keeps /dev/nvme0n1 from claiming
// /dev/nvme0n11.
func devicesOnDisk(disk string) map[string]bool {
	out := map[string]bool{disk: true}
	parts, _ := kernelPartitions(disk)
	for _, p := range parts {
		out[partitionDevice(disk, p)] = true
	}
	return out
}

// mountsOnDisk returns the filesystems mounted from a disk, deepest mountpoint
// first so a nested mount is released before the one it sits inside.
func mountsOnDisk(disk string) []diskMount {
	devs := devicesOnDisk(disk)
	var out []diskMount
	for _, fields := range procTable(procMountsPath, 0) {
		if len(fields) >= 2 && devs[fields[0]] {
			out = append(out, diskMount{Device: fields[0], Mountpoint: fields[1]})
		}
	}
	slices.SortFunc(out, func(a, b diskMount) int {
		return strings.Count(b.Mountpoint, "/") - strings.Count(a.Mountpoint, "/")
	})
	return out
}

// swapsOnDisk returns the active swap areas backed by a disk.
func swapsOnDisk(disk string) []string {
	devs := devicesOnDisk(disk)
	var out []string
	// The first line of /proc/swaps is a column header, not an entry.
	for _, fields := range procTable(procSwapsPath, 1) {
		if len(fields) >= 1 && devs[fields[0]] {
			out = append(out, fields[0])
		}
	}
	return out
}

// procTable splits a whitespace-delimited /proc table into fields per line,
// dropping the first skip lines and any blank ones.
func procTable(path string, skip int) [][]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out [][]string
	n := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		n++
		if n <= skip {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			out = append(out, fields)
		}
	}
	return out
}

// byIDPreference orders the /dev/disk/by-id/ namespaces we prefer to name a
// pool member by. wwn- ids are last: they are stable but opaque, so a degraded
// pool reports a number nobody can map to a drive bay.
var byIDPreference = []string{"nvme-", "ata-", "scsi-", "virtio-", "usb-", "wwn-"}

// buildByIDIndex maps kernel device name to its preferred by-id path.
func buildByIDIndex() map[string]string {
	entries, err := os.ReadDir(devByIDDir)
	if err != nil {
		return map[string]string{}
	}
	// Track the chosen preference rank per device so a later, less-preferred
	// id cannot displace a better one already found.
	rank := map[string]int{}
	out := map[string]string{}
	for _, e := range entries {
		id := e.Name()
		// Partition links describe a slice of the disk, not the disk.
		if strings.Contains(id, "-part") {
			continue
		}
		target, err := filepath.EvalSymlinks(filepath.Join(devByIDDir, id))
		if err != nil {
			continue
		}
		dev := filepath.Base(target)
		r := prefixRank(id)
		if existing, ok := rank[dev]; ok && existing <= r {
			continue
		}
		rank[dev] = r
		out[dev] = filepath.Join(devByIDDir, id)
	}
	return out
}

func prefixRank(id string) int {
	for i, p := range byIDPreference {
		if strings.HasPrefix(id, p) {
			return i
		}
	}
	return len(byIDPreference)
}

// liveMediaDevices returns the set of parent disk names backing the live
// installer's own media, so they can never be selected as install targets.
func liveMediaDevices() map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(procMountsPath)
	if err != nil {
		return out
	}
	liveMounts := []string{"/cdrom", "/run/live/medium", "/lib/live/mount/medium"}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		if !slices.Contains(liveMounts, fields[1]) {
			continue
		}
		if parent := parentDisk(filepath.Base(fields[0])); parent != "" {
			out[parent] = true
		}
	}
	return out
}

// parentDisk resolves a partition's kernel name to its whole-disk name by
// walking /sys/class/block/<part>/.. — the sysfs parent of a partition is its
// disk. Returns the input unchanged when it is already a whole disk.
func parentDisk(dev string) string {
	link, err := filepath.EvalSymlinks(filepath.Join(sysClassBlockDir, dev))
	if err != nil {
		return dev
	}
	// A whole disk lives directly under a bus path; a partition sits one level
	// deeper, inside its disk's directory.
	if _, err := os.Stat(filepath.Join(link, "partition")); err != nil {
		return dev
	}
	return filepath.Base(filepath.Dir(link))
}

// describeContent summarises what is currently on a disk for the confirmation
// screen. Best-effort: an unreadable disk is reported as unknown, not empty,
// because "empty" invites an operator to erase something that is not.
func describeContent(dev string) string {
	out, err := exec.Command("blkid", "-p", "-o", "value", "-s", "TYPE", dev).Output()
	if err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t + " filesystem"
		}
	}
	// No filesystem on the whole device: report the partition table instead,
	// since that is what tells the operator the disk is in use.
	out, err = exec.Command("blkid", "-p", "-o", "value", "-s", "PTTYPE", dev).Output()
	if err != nil {
		return "unknown"
	}
	if t := strings.TrimSpace(string(out)); t != "" {
		return t + " partition table"
	}
	return "empty"
}

func sysfsString(dev, file string) string {
	data, err := os.ReadFile(filepath.Join(sysBlockDir, dev, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func sysfsInt(dev, file string) int64 {
	n, err := strconv.ParseInt(sysfsString(dev, file), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func sysfsIntDefault(dev, file string, def int) int {
	if n := sysfsInt(dev, file); n > 0 {
		return int(n)
	}
	return def
}

func hasAnyPrefix(s string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(s, p) })
}
