package install

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ARC bounds. Spinifex RAM is guest RAM: Proxmox defaults non-PVE products to
// half of system memory, which on a 256GB compute node would remove 128GB of
// schedulable instance capacity. 10% capped at 16GiB is their PVE behaviour and
// the right trade for a hypervisor.
const (
	arcMinMiB     = 64
	arcMaxCapMiB  = 16 * 1024
	arcFraction   = 0.10
	arcHeadroomMi = 1024 // always leave the OS a GiB above the ARC ceiling
)

// zfsDataset describes one dataset created at install time. Order matters:
// parents must precede children so mountpoints nest correctly.
type zfsDataset struct {
	name       string
	mountpoint string
	props      []string
}

// datasets is the layout described in the plan. Recordsizes are matched to each
// service's access pattern; 16K on the predastore index keeps RAIDZ-1 parity
// overhead at 33% rather than the ~50% a 4-8K record would cost.
var datasets = []zfsDataset{
	{ZFSPoolName + "/ROOT", "none", []string{"canmount=off"}},
	{ZFSRootDataset, "/", []string{"acltype=posix"}},
	{ZFSPoolName + "/data", "/var/lib/spinifex", []string{"canmount=off"}},
	{ZFSPoolName + "/data/viperblock", "/var/lib/spinifex/viperblock",
		[]string{"recordsize=128K", "logbias=throughput"}},
	{ZFSPoolName + "/data/predastore", "/var/lib/spinifex/predastore",
		[]string{"recordsize=1M"}},
	// Broken out so the later move of object data onto dedicated drives is a
	// zfs send | zfs recv rather than an rsync with downtime.
	{ZFSPoolName + "/data/predastore-nodes", "/var/lib/spinifex/predastore/distributed/nodes", nil},
	// BadgerDB index plus the Raft BoltDB: small random, fsync-heavy.
	{ZFSPoolName + "/data/predastore-db", "/var/lib/spinifex/predastore/distributed/db",
		[]string{"recordsize=16K", "logbias=latency"}},
	{ZFSPoolName + "/data/nats", "/var/lib/spinifex/nats", []string{"recordsize=16K"}},
	{ZFSPoolName + "/log", "/var/log/spinifex", []string{"compression=zstd"}},
}

// buildVdev renders the vdev specification for zpool create. Pure: it is the
// function that decides pool shape, so it is exhaustively unit-tested rather
// than discovered to be wrong on hardware.
func buildVdev(cfg DiskConfig) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	if !cfg.FS.IsZFS() {
		return "", fmt.Errorf("buildVdev: %s is not a zfs topology", cfg.FS)
	}

	// Members are named by their partition-3 by-id path: a pool built on
	// /dev/sdX breaks the first time a controller enumerates in a new order.
	members := make([]string, len(cfg.Disks))
	for i, d := range cfg.Disks {
		members[i] = d.StablePartitionPath(rootPartNum)
	}

	keyword := cfg.FS.vdevKeyword()
	if keyword == "" {
		return strings.Join(members, " "), nil // plain stripe
	}
	// RAID10 is a stripe of two-disk mirrors, paired in selection order — which
	// is why the UI numbers selected disks instead of just ticking them.
	if cfg.FS == FSZFSRAID10 {
		var parts []string
		for i := 0; i+1 < len(members); i += 2 {
			parts = append(parts, fmt.Sprintf("mirror %s %s", members[i], members[i+1]))
		}
		return strings.Join(parts, " "), nil
	}
	return keyword + " " + strings.Join(members, " "), nil
}

// detectAshift derives the pool sector size from the widest sector any member
// reports. ashift cannot be changed after creation, and one that is too small
// costs read-modify-write on every block for the life of the pool, so the floor
// is 12 (4K) even when every disk claims 512.
func detectAshift(disks []Disk) int {
	widest := 4096
	for _, d := range disks {
		widest = max(widest, d.PhysicalBlockSize, d.LogicalBlockSize)
	}
	// ashift is the base-2 exponent of the sector size.
	return bits.Len(uint(widest)) - 1
}

// defaultARCMaxMiB computes the ARC ceiling from total system memory.
func defaultARCMaxMiB(totalMiB int) int {
	want := int(math.Round(float64(totalMiB) * arcFraction))
	want = min(want, arcMaxCapMiB)
	// Never let the ARC crowd the OS out of its last GiB on a small node.
	if ceiling := totalMiB - arcHeadroomMi; ceiling > 0 {
		want = min(want, ceiling)
	}
	return max(want, arcMinMiB)
}

// totalMemoryMiB reads MemTotal from /proc/meminfo.
func totalMemoryMiB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		var kb int
		if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &kb); err == nil {
			return kb / 1024
		}
	}
	return 0
}

// resolveZFSOpts fills unset advanced options with their computed defaults, so
// every value written to the pool is explicit and auditable afterwards.
func resolveZFSOpts(cfg DiskConfig) ZFSOpts {
	o := cfg.ZFS
	if o.Ashift == 0 {
		o.Ashift = detectAshift(cfg.Disks)
	}
	if o.Compress == "" {
		// Named explicitly rather than "on" so a future OpenZFS default change
		// cannot silently alter what this pool does.
		o.Compress = "lz4"
	}
	if o.Checksum == "" {
		o.Checksum = "on"
	}
	if o.Copies == 0 {
		o.Copies = 1
	}
	if o.ARCMaxMiB == 0 {
		o.ARCMaxMiB = defaultARCMaxMiB(totalMemoryMiB())
	}
	return o
}

// loadZFSModule brings up the ZFS kernel module in the live environment and
// waits for /dev/zfs. The module is prebuilt against the shipped kernel at ISO
// build time, so failure here means the ISO is broken, not the hardware.
func loadZFSModule() error {
	if err := run("modprobe", "zfs"); err != nil {
		slog.Warn("modprobe zfs failed, checking for /dev/zfs anyway", "err", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat("/dev/zfs"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("/dev/zfs did not appear after modprobe zfs — the ZFS module is missing or does not match the running kernel")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ImportablePools returns the names of pools visible to `zpool import`. There
// is still no machine-readable output in OpenZFS 2.3, so the human format is
// parsed — the same approach Proxmox::Sys::ZFS takes.
func ImportablePools() ([]string, error) {
	out, err := exec.Command("zpool", "import").Output()
	if err != nil {
		// zpool exits non-zero when there is nothing to import, which is the
		// normal case. Anything else — no binary, no /dev/zfs — must surface, or
		// a machine with an existing pool looks empty to the caller.
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, nil
		}
		return nil, fmt.Errorf("zpool import: %w", err)
	}
	return parseImportablePools(string(out)), nil
}

// parseImportablePools extracts pool names from `zpool import` output. Split
// out from the command call so it can be tested against captured fixtures.
func parseImportablePools(out string) []string {
	var pools []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Only the "pool:" key introduces a pool; "id:", "state:" and the
		// config tree that follows are attributes of the one before it.
		if name, ok := strings.CutPrefix(line, "pool:"); ok {
			if n := strings.TrimSpace(name); n != "" {
				pools = append(pools, n)
			}
		}
	}
	return pools
}

// createPool builds the root pool across the selected disks. altroot keeps
// every dataset mounted under the install target instead of over the live
// filesystem, and cachefile=none stops the live environment from claiming it.
func createPool(cfg DiskConfig, opts ZFSOpts) error {
	vdev, err := buildVdev(cfg)
	if err != nil {
		return err
	}

	args := []string{
		"create", "-f",
		"-o", "cachefile=none",
		"-o", fmt.Sprintf("ashift=%d", opts.Ashift),
		"-O", "atime=on", "-O", "relatime=on",
		"-O", "xattr=sa",
		"-O", fmt.Sprintf("compression=%s", opts.Compress),
		"-O", fmt.Sprintf("checksum=%s", opts.Checksum),
		"-O", fmt.Sprintf("copies=%d", opts.Copies),
		"-O", "mountpoint=none",
		"-R", mountRoot,
		ZFSPoolName,
	}
	args = append(args, strings.Fields(vdev)...)
	if err := run("zpool", args...); err != nil {
		return fmt.Errorf("zpool create: %w", err)
	}

	// TRIM matters on a pool that churns EBS volumes: without it the drives'
	// garbage collection degrades write latency over months, and diagnosing
	// that after the fact is miserable. Detected, never prompted.
	if allFlash(cfg.Disks) {
		if err := run("zpool", "set", "autotrim=on", ZFSPoolName); err != nil {
			slog.Warn("could not enable autotrim", "err", err)
		}
	}
	return nil
}

// allFlash reports whether every member is non-rotational.
func allFlash(disks []Disk) bool {
	for _, d := range disks {
		if d.Rotational {
			return false
		}
	}
	return len(disks) > 0
}

// createDatasets lays out the service datasets. Mountpoints are set relative to
// the pool's altroot, so they land under the install target now and at their
// real paths once the installed system imports the pool.
func createDatasets() error {
	for _, ds := range datasets {
		args := []string{"create", "-o", "mountpoint=" + ds.mountpoint}
		for _, p := range ds.props {
			args = append(args, "-o", p)
		}
		args = append(args, ds.name)
		if err := run("zfs", args...); err != nil {
			return fmt.Errorf("create dataset %s: %w", ds.name, err)
		}
	}
	return nil
}

// datasetProtectFilters keeps rsync --delete away from the dataset mountpoints.
//
// The datasets are mounted before the copy so files land on the right one, but
// the live source has nothing at those paths, so --delete tries to rmdir a busy
// mountpoint and fails the whole transfer. Protect rules exempt them.
func datasetProtectFilters(cfg DiskConfig) []string {
	if !cfg.FS.IsZFS() {
		return nil
	}
	var out []string
	for _, ds := range datasets {
		if ds.mountpoint == "/" || ds.mountpoint == "none" {
			continue
		}
		out = append(out, "--filter=P "+ds.mountpoint)
	}
	return out
}

// writeZFSSystemConfig writes the target's ZFS tunables: the ARC ceiling, and
// the matching host reserve so the daemon's admission gate stops handing out
// memory the ARC is holding.
func writeZFSSystemConfig(opts ZFSOpts) error {
	modprobeDir := filepath.Join(mountRoot, "etc/modprobe.d")
	if err := os.MkdirAll(modprobeDir, 0o755); err != nil {
		return err
	}
	arcBytes := int64(opts.ARCMaxMiB) << 20
	conf := fmt.Sprintf("# Generated by the Spinifex installer.\n"+
		"# Guest RAM is the scarce resource on a hypervisor, so the ARC is\n"+
		"# capped well below the ZFS default of half of system memory.\n"+
		"options zfs zfs_arc_max=%d\n", arcBytes)
	if err := os.WriteFile(filepath.Join(modprobeDir, "zfs.conf"), []byte(conf), 0o644); err != nil {
		return fmt.Errorf("write zfs.conf: %w", err)
	}

	// The daemon computes schedulable memory as host total minus this reserve
	// (defaultHostReserve in daemon/resource.go). ARC is invisible to it, so
	// the ceiling has to be declared or instances get admitted into memory the
	// ARC already owns.
	reserve := defaultHostReserveGB + float64(opts.ARCMaxMiB)/1024.0
	env := fmt.Sprintf("# Generated by the Spinifex installer.\n"+
		"# %.1fGB base reserve + %.1fGB ZFS ARC ceiling.\n"+
		"SPINIFEX_RESERVED_MEM_GB=%.1f\n",
		defaultHostReserveGB, float64(opts.ARCMaxMiB)/1024.0, reserve)
	confDir := filepath.Join(mountRoot, "etc/spinifex")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(confDir, "host.env"), []byte(env), 0o644)
}

// defaultHostReserveGB mirrors defaultHostReserve.memGB in
// spinifex/daemon/resource.go. Changing one without the other silently
// misreports how much memory a ZFS node can schedule.
const defaultHostReserveGB = 2.0

// exportPool releases the pool so a retry after a failed install is not blocked
// by "pool is busy". The live environment runs spinifex-init as PID 1, so
// nothing else will ever clean this up.
func exportPool() {
	if err := run("zpool", "export", ZFSPoolName); err != nil {
		slog.Warn("zpool export failed", "pool", ZFSPoolName, "err", err)
	}
}

// clearDisk takes a disk over unconditionally, whatever it holds: everything
// attached to it is released, every signature on it is wiped, its partition
// table is erased, and the kernel is made to agree the disk is now empty.
//
// The last part is the point. sgdisk writes to the platters and exits 0 even
// when the kernel refuses to drop the old table, so without the assertion an
// install proceeds against partition nodes describing the previous layout.
//
// The probes are quiet because a blank disk makes all of them fail, and their
// stderr on the install console reads like the install is going wrong.
func clearDisk(d Disk) error {
	releaseDisk(d)

	// Every partition, not just the root slice. ZFS labels live at both ends of
	// a device and wipefs does not know about them, so a data drive that was
	// once a pool member keeps its label and fails a later zpool create.
	parts, err := kernelPartitions(d.Path)
	if err != nil {
		return err
	}
	for _, p := range parts {
		dev := partitionDevice(d.Path, p)
		_ = runQuiet("zpool", "labelclear", "-f", dev)
		_ = runQuiet("wipefs", "-a", dev)
	}

	_ = runQuiet("wipefs", "-a", d.Path)
	if err := runQuiet("sgdisk", "-Z", d.Path); err != nil {
		return fmt.Errorf("erase partition table: %w", err)
	}
	return settlePartitions(d)
}
