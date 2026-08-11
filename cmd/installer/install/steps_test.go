package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeExec records every command the install steps shell out to and lets a
// test fail a chosen one. Without it these steps can only be exercised by
// running a real install against real hardware.
type fakeExec struct {
	calls []string
	fail  func(name string, args []string) error

	// parts is the kernel's partition view per disk. Seeded by the test and then
	// moved by the sgdisk calls the installer makes, so the partition-table
	// assertions can run without a block device.
	parts map[string][]string

	// stale marks disks whose view never moves however many tables are written
	// to them: something holds the disk open, BLKRRPART returns EBUSY, and the
	// kernel keeps serving the old table while sgdisk reports success.
	stale map[string]bool
}

// holdOpen makes a disk keep its current partition view forever.
func (f *fakeExec) holdOpen(disk string) { f.stale[disk] = true }

// seedPartitions gives a disk a pre-existing partition table — the state a
// drive is in when the node has been installed on before.
func (f *fakeExec) seedPartitions(disk string, nums ...int) {
	for _, n := range nums {
		f.parts[disk] = append(f.parts[disk], filepath.Base(partitionPath(disk, n)))
	}
}

// applySgdisk moves the fake kernel view in step with a table rewrite, the way
// a kernel that honoured every BLKRRPART would: -Z empties it, each -n adds the
// partition it creates.
func (f *fakeExec) applySgdisk(args []string) {
	if len(args) == 0 {
		return
	}
	disk := args[len(args)-1]
	if f.stale[disk] {
		return
	}
	for i, a := range args {
		switch {
		case a == "-Z":
			f.parts[disk] = nil
		case a == "-n" && i+1 < len(args):
			num, _, _ := strings.Cut(args[i+1], ":")
			n, err := strconv.Atoi(num)
			if err != nil {
				continue
			}
			f.parts[disk] = append(f.parts[disk], filepath.Base(partitionPath(disk, n)))
		}
	}
}

func fakeCommands(t *testing.T) *fakeExec {
	t.Helper()
	f := &fakeExec{parts: map[string][]string{}, stale: map[string]bool{}}
	prevRun, prevQuiet, prevEnv, prevParts := run, runQuiet, runEnv, kernelPartitions

	record := func(name string, args ...string) error {
		f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		if f.fail != nil {
			if err := f.fail(name, args); err != nil {
				return err
			}
		}
		if name == "sgdisk" {
			f.applySgdisk(args)
		}
		return nil
	}
	run = record
	runQuiet = record
	runEnv = func(env []string, name string, args ...string) error {
		return record(name, append(env, args...)...)
	}
	kernelPartitions = func(disk string) ([]string, error) {
		got := slices.Clone(f.parts[disk])
		slices.Sort(got)
		return got, nil
	}

	// A short settle so a test asserting the stale-table failure does not wait
	// out the install's real budget.
	prevTimeout := partitionSettleTimeout
	partitionSettleTimeout = 10 * time.Millisecond

	t.Cleanup(func() {
		run, runQuiet, runEnv, kernelPartitions = prevRun, prevQuiet, prevEnv, prevParts
		partitionSettleTimeout = prevTimeout
	})
	return f
}

// ran reports whether any recorded command contains every given fragment.
func (f *fakeExec) ran(fragments ...string) bool {
	for _, c := range f.calls {
		all := true
		for _, frag := range fragments {
			if !strings.Contains(c, frag) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func (f *fakeExec) mustRun(t *testing.T, fragments ...string) {
	t.Helper()
	if !f.ran(fragments...) {
		t.Errorf("no command matched %v; recorded:\n  %s", fragments, strings.Join(f.calls, "\n  "))
	}
}

func (f *fakeExec) mustNotRun(t *testing.T, fragments ...string) {
	t.Helper()
	if f.ran(fragments...) {
		t.Errorf("unexpected command matching %v; recorded:\n  %s", fragments, strings.Join(f.calls, "\n  "))
	}
}

// indexOf returns the position of the first command matching frag, or -1.
func (f *fakeExec) indexOf(frag string) int {
	for i, c := range f.calls {
		if strings.Contains(c, frag) {
			return i
		}
	}
	return -1
}

// indexOfExact is indexOf for a whole command, for cases where one command
// line is a prefix of another.
func (f *fakeExec) indexOfExact(cmd string) int {
	for i, c := range f.calls {
		if c == cmd {
			return i
		}
	}
	return -1
}

// withMountRoot points the install target at a temp tree.
func withMountRoot(t *testing.T) string {
	t.Helper()
	prev := mountRoot
	mountRoot = t.TempDir()
	t.Cleanup(func() { mountRoot = prev })
	return mountRoot
}

// targetSkeleton creates the directories rsync would already have copied in by
// the time these steps run.
func targetSkeleton(t *testing.T, root string) {
	t.Helper()
	for _, d := range []string{"etc/default", "etc/grub.d", "etc/systemd/system", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// tempDisks builds disks whose device nodes are real files, so the steps that
// wait for partition nodes to appear can complete.
// fakeProcTables points the mount and swap readers at fixture tables for the
// duration of a test.
func fakeProcTables(t *testing.T, mounts, swaps string) {
	t.Helper()
	dir := t.TempDir()
	point := func(dst *string, name, content string) {
		prev := *dst
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		*dst = p
		t.Cleanup(func() { *dst = prev })
	}
	point(&procMountsPath, "mounts", mounts)
	point(&procSwapsPath, "swaps", swaps)
}

func tempDisks(t *testing.T, n int, gib int64) []Disk {
	t.Helper()
	dir := t.TempDir()
	out := make([]Disk, n)
	for i := range out {
		d := Disk{
			Path:              filepath.Join(dir, fmt.Sprintf("sd%c", 'a'+i)),
			Bytes:             gib << 30,
			LogicalBlockSize:  512,
			PhysicalBlockSize: 512,
		}
		for _, part := range []int{biosPartNum, espPartNum, rootPartNum} {
			if err := os.WriteFile(d.PartitionPath(part), nil, 0o644); err != nil {
				t.Fatalf("create partition node: %v", err)
			}
		}
		out[i] = d
	}
	return out
}

func TestPartitionDisksWritesTheSameLayoutOnEveryDisk(t *testing.T) {
	f := fakeCommands(t)
	cfg := DiskConfig{FS: FSZFSRAID1, Disks: tempDisks(t, 2, 200)}

	if err := partitionDisks(cfg); err != nil {
		t.Fatalf("partitionDisks: %v", err)
	}

	for _, d := range cfg.Disks {
		// Stale signatures survive a new table, so the wipe comes first.
		f.mustRun(t, "wipefs -a "+d.Path)
		f.mustRun(t, "sgdisk -Z "+d.Path)
		f.mustRun(t,
			"sgdisk", d.Path,
			"-n 1:1M:+1M", "-t 1:EF02", "-c 1:bios_boot",
			// A 200GiB disk is a real host, so it gets the large ESP.
			"-n 2:0:+1024M", "-t 2:EF00", "-c 2:ESP",
			"-n 3:0:-1024M", "-t 3:BF00", "-c 3:root",
		)
		f.mustRun(t, "sgdisk -G "+d.Path)
		f.mustRun(t, "partprobe "+d.Path)

		if wipe, part := f.indexOf("wipefs -a "+d.Path), f.indexOf("-c 3:root "+d.Path); wipe > part {
			t.Errorf("wipefs ran after the table was written (%d > %d)", wipe, part)
		}
	}
	f.mustRun(t, "udevadm settle --timeout=10")
}

func TestPartitionDisksTypesTheRootPartitionForTheFilesystem(t *testing.T) {
	f := fakeCommands(t)
	if err := partitionDisks(DiskConfig{FS: FSExt4, Disks: tempDisks(t, 1, 40)}); err != nil {
		t.Fatalf("partitionDisks: %v", err)
	}
	f.mustRun(t, "-t 3:8300")
	f.mustNotRun(t, "-t 3:BF00")
	// Under 100GiB the ESP stays at the smaller size.
	f.mustRun(t, "-n 2:0:+512M")
}

func TestPartitionDisksNamesTheDiskThatFailed(t *testing.T) {
	f := fakeCommands(t)
	disks := tempDisks(t, 2, 40)
	f.fail = func(name string, args []string) error {
		if name == "sgdisk" && strings.Contains(strings.Join(args, " "), "bios_boot") {
			return errors.New("device busy")
		}
		return nil
	}

	err := partitionDisks(DiskConfig{FS: FSExt4, Disks: disks})
	if err == nil || !strings.Contains(err.Error(), disks[0].Path) {
		t.Fatalf("err = %v, want it to name %s", err, disks[0].Path)
	}
}

func TestPartitionDisksSurvivesBestEffortFailures(t *testing.T) {
	f := fakeCommands(t)
	// partprobe, udevadm and the GUID randomisation are all advisory: sgdisk
	// has already issued BLKRRPART and the nodes exist.
	f.fail = func(name string, args []string) error {
		switch {
		case name == "partprobe", name == "udevadm":
			return errors.New("not available")
		case name == "sgdisk" && len(args) > 0 && args[0] == "-G":
			return errors.New("no")
		}
		return nil
	}
	if err := partitionDisks(DiskConfig{FS: FSExt4, Disks: tempDisks(t, 1, 40)}); err != nil {
		t.Fatalf("partitionDisks: %v", err)
	}
}

func TestWaitForPathFailsWhenTheNodeNeverAppears(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "sda3")

	err := waitForPath(missing, time.Now().Add(-time.Second))
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("err = %v, want a timeout naming the missing node", err)
	}
}

func TestWaitForPathReturnsOnceTheNodeExists(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "node")
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForPath(f.Name(), time.Now().Add(-time.Second)); err != nil {
		t.Errorf("waitForPath on an existing node: %v", err)
	}
}

func TestEspSizeMiB(t *testing.T) {
	if got := espSizeMiB(Disk{Bytes: 99 << 30}); got != espSmallMiB {
		t.Errorf("99GiB -> %d, want %d", got, espSmallMiB)
	}
	if got := espSizeMiB(Disk{Bytes: 100 << 30}); got != espLargeMiB {
		t.Errorf("100GiB -> %d, want %d", got, espLargeMiB)
	}
}

// A drive arriving with a previous install's layout must have every partition
// cleared, not just the one the root filesystem used to be on: a data drive's
// partition is p1, so a p3-only labelclear leaves a former pool member's ZFS
// label in place at both ends of the device.
func TestClearDiskWipesEveryPartitionItFinds(t *testing.T) {
	f := fakeCommands(t)
	d := tempDisks(t, 1, 40)[0]
	f.seedPartitions(d.Path, biosPartNum, espPartNum, rootPartNum)

	if err := clearDisk(d); err != nil {
		t.Fatalf("clearDisk: %v", err)
	}
	for _, n := range []int{biosPartNum, espPartNum, rootPartNum} {
		f.mustRun(t, "zpool labelclear -f "+d.PartitionPath(n))
		f.mustRun(t, "wipefs -a "+d.PartitionPath(n))
	}
	f.mustRun(t, "wipefs -a "+d.Path)
	f.mustRun(t, "sgdisk -Z "+d.Path)
}

func TestClearDiskProbesNoPartitionsOnABlankDrive(t *testing.T) {
	f := fakeCommands(t)
	blank := Disk{Path: filepath.Join(t.TempDir(), "sdb")}

	if err := clearDisk(blank); err != nil {
		t.Fatalf("clearDisk: %v", err)
	}
	f.mustNotRun(t, "labelclear")
	f.mustRun(t, "wipefs -a "+blank.Path)
}

// The reported field failure: a drive whose old table the kernel never dropped.
// Proceeding would format the previous layout's offsets, so the install must
// stop, and the message must say the table is stale rather than blaming the
// device node that did not appear.
func TestClearDiskFailsWhenTheKernelKeepsTheOldTable(t *testing.T) {
	f := fakeCommands(t)
	d := tempDisks(t, 1, 40)[0]
	f.seedPartitions(d.Path, biosPartNum, espPartNum, rootPartNum)
	// sgdisk writes to the platters and exits 0, but BLKRRPART returned EBUSY,
	// so the kernel's view never changes.
	f.holdOpen(d.Path)

	err := clearDisk(d)
	if err == nil {
		t.Fatal("clearDisk succeeded against a stale partition table")
	}
	for _, want := range []string{d.Path, "still shows partitions", "previous layout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

func TestClearDiskReleasesMountsAndSwapFirst(t *testing.T) {
	f := fakeCommands(t)
	d := tempDisks(t, 1, 40)[0]
	f.seedPartitions(d.Path, biosPartNum, espPartNum, rootPartNum)

	root, esp := d.PartitionPath(rootPartNum), d.PartitionPath(espPartNum)
	swapDev := d.PartitionPath(biosPartNum)
	fakeProcTables(t,
		fmt.Sprintf("%s /oldroot ext4 rw 0 0\n%s /oldroot/boot/efi vfat rw 0 0\n", root, esp),
		fmt.Sprintf("Filename\t\t\t\tType\t\tSize\tUsed\tPriority\n%s partition 1000 0 -2\n", swapDev),
	)

	if err := clearDisk(d); err != nil {
		t.Fatalf("clearDisk: %v", err)
	}
	f.mustRun(t, "swapoff "+swapDev)

	// Deepest mountpoint first: unmounting /oldroot while /oldroot/boot/efi is
	// still attached fails with EBUSY and leaves the disk held.
	var order []string
	for _, c := range f.calls {
		if c == "umount "+root || c == "umount "+esp {
			order = append(order, c)
		}
	}
	if want := []string{"umount " + esp, "umount " + root}; !slices.Equal(order, want) {
		t.Errorf("unmount order:\n got %v\nwant %v", order, want)
	}
}

// A drive held by md or LVM cannot be released — the ISO ships neither tool —
// so the operator has to be told what is holding it.
func TestStaleTableErrorNamesTheHolder(t *testing.T) {
	f := newFakeSysfs(t, fakeDisk{name: "sda", sectors: 1 << 20, partitions: []string{"sda1"}})
	f.addHolder("sda", "sda1", "md0")

	prev := kernelPartitions
	kernelPartitions = readKernelPartitions
	t.Cleanup(func() { kernelPartitions = prev })

	err := staleTableError(Disk{Path: "/dev/sda"}, nil, []string{"sda1"})
	if err == nil || !strings.Contains(err.Error(), "held by md0") {
		t.Fatalf("err = %v, want it to name the md0 holder", err)
	}
}

// The reported field failure as a unit test: two drives arrive carrying a
// previous install's partitions and the third has never been partitioned. All
// three must be taken over, and the blank one must not be the only one to get
// a new table.
func TestPartitionDisksTakesOverDrivesThatAlreadyHavePartitions(t *testing.T) {
	f := fakeCommands(t)
	nodes := tempDisks(t, 3, 200)
	f.seedPartitions(nodes[0].Path, biosPartNum, espPartNum, rootPartNum) // held an OS
	f.seedPartitions(nodes[1].Path, dataPartNum)                          // held data
	// nodes[2] arrives blank.

	cfg := DiskConfig{FS: FSExt4}.WithRoles([]RoleMount{
		{Role: RoleOS, Disk: nodes[0]},
		{Role: RoleSpinifex, Disk: nodes[1]},
		{Role: RolePredastore, Disk: nodes[2]},
	})
	if err := partitionDisks(cfg); err != nil {
		t.Fatalf("partitionDisks: %v", err)
	}
	for _, d := range nodes {
		f.mustRun(t, "sgdisk -Z "+d.Path)
	}
	f.mustRun(t, "sgdisk", nodes[0].Path, "-n 3:0:-1024M")
	for _, d := range nodes[1:] {
		f.mustRun(t, "sgdisk", d.Path, "-n 1:1M:-1024M", "-t 1:8300")
	}
}

// A previous OS install leaves p1, p2, p3 — exactly the numbering the boot
// layout writes. Asserting the expected partitions exist would pass on those
// stale nodes and the install would format the old offsets, so the erase-stage
// assertion on an empty table is what has to catch it.
func TestPartitionOneStopsWhenAnOldTableHasTheSameLayout(t *testing.T) {
	f := fakeCommands(t)
	d := tempDisks(t, 1, 200)[0]
	f.seedPartitions(d.Path, biosPartNum, espPartNum, rootPartNum)
	f.holdOpen(d.Path)

	err := partitionOne(d, typeLinuxFS)
	if err == nil {
		t.Fatal("partitionOne succeeded against a stale partition table")
	}
	// Specifically the erase-stage message: reaching the post-write check would
	// mean the stale nodes had already been accepted.
	if !strings.Contains(err.Error(), "still shows partitions") {
		t.Errorf("err = %q, want the erase-stage stale-table message", err)
	}
	f.mustNotRun(t, "sgdisk -n")
}

func TestFormatESPsLabelsEveryMember(t *testing.T) {
	f := fakeCommands(t)
	cfg := DiskConfig{FS: FSZFSRAID1, Disks: tempDisks(t, 2, 40)}
	if err := formatESPs(cfg); err != nil {
		t.Fatalf("formatESPs: %v", err)
	}
	for _, d := range cfg.Disks {
		f.mustRun(t, "mkfs.fat -F 32 -n EFI "+d.PartitionPath(espPartNum))
	}
}

func TestFormatESPsNamesTheFailingESP(t *testing.T) {
	f := fakeCommands(t)
	cfg := DiskConfig{FS: FSZFSRAID1, Disks: tempDisks(t, 2, 40)}
	f.fail = func(name string, _ []string) error {
		if name == "mkfs.fat" {
			return errors.New("no such device")
		}
		return nil
	}
	err := formatESPs(cfg)
	if err == nil || !strings.Contains(err.Error(), cfg.Disks[0].PartitionPath(espPartNum)) {
		t.Fatalf("err = %v, want it to name the ESP", err)
	}
}

func TestFormatPartitionsMakesTheExt4Root(t *testing.T) {
	f := fakeCommands(t)
	cfg := DiskConfig{FS: FSExt4, Disks: tempDisks(t, 1, 40)}
	if err := formatPartitions(cfg); err != nil {
		t.Fatalf("formatPartitions: %v", err)
	}
	f.mustRun(t, "mkfs.ext4 -F "+cfg.Disks[0].PartitionPath(rootPartNum))
}

func TestMountPartitionsMountsRootThenESP(t *testing.T) {
	root := withMountRoot(t)
	f := fakeCommands(t)
	cfg := DiskConfig{FS: FSExt4, Disks: tempDisks(t, 1, 40)}

	if err := mountPartitions(cfg); err != nil {
		t.Fatalf("mountPartitions: %v", err)
	}
	d := cfg.Disks[0]
	f.mustRun(t, "mount "+d.PartitionPath(rootPartNum)+" "+root)
	f.mustRun(t, "mount "+d.PartitionPath(espPartNum)+" "+efiPart())
	if _, err := os.Stat(efiPart()); err != nil {
		t.Errorf("ESP mountpoint was not created: %v", err)
	}
	if f.indexOf("mount "+d.PartitionPath(rootPartNum)) > f.indexOf("mount "+d.PartitionPath(espPartNum)) {
		t.Error("the ESP was mounted before the root it lives under")
	}
}

func TestMountPartitionsStopsWhenTheRootWillNotMount(t *testing.T) {
	withMountRoot(t)
	f := fakeCommands(t)
	f.fail = func(name string, _ []string) error {
		if name == "mount" {
			return errors.New("wrong fs type")
		}
		return nil
	}
	if err := mountPartitions(DiskConfig{FS: FSExt4, Disks: tempDisks(t, 1, 40)}); err == nil {
		t.Fatal("expected an error when the root mount fails")
	}
	f.mustNotRun(t, "boot/efi")
}

func TestCleanupTargetExportsThePoolOnZFS(t *testing.T) {
	root := withMountRoot(t)
	f := fakeCommands(t)

	cleanupTarget(DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)})

	// Recursive: one dataset left mounted is enough to block the export.
	f.mustRun(t, "umount -R "+root)
	f.mustRun(t, "zpool export "+ZFSPoolName)
	if f.indexOf("umount -R") > f.indexOf("zpool export") {
		t.Error("the pool was exported before its datasets were unmounted")
	}
}

func TestCleanupTargetUnmountsESPBeforeRootOnExt4(t *testing.T) {
	root := withMountRoot(t)
	f := fakeCommands(t)

	cleanupTarget(DiskConfig{FS: FSExt4, Disks: disks(1, 40)})

	f.mustNotRun(t, "zpool export")
	esp, rootIdx := f.indexOfExact("umount "+efiPart()), f.indexOfExact("umount "+root)
	if esp < 0 || rootIdx < 0 {
		t.Fatalf("missing umounts; recorded:\n  %s", strings.Join(f.calls, "\n  "))
	}
	if esp > rootIdx {
		t.Error("the root was unmounted before the ESP nested inside it")
	}
}

func TestCreatePoolBuildsTheZpoolCommand(t *testing.T) {
	root := withMountRoot(t)
	f := fakeCommands(t)
	cfg := DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)}
	opts := ZFSOpts{Ashift: 12, Compress: "lz4", Checksum: "blake3", Copies: 1, ARCMaxMiB: 4096}

	if err := createPool(cfg, opts); err != nil {
		t.Fatalf("createPool: %v", err)
	}
	f.mustRun(t, "zpool create -f",
		"cachefile=none", "ashift=12", "compression=lz4", "checksum=blake3", "copies=1",
		// altroot keeps the datasets under the target rather than over the live system.
		"-R "+root, ZFSPoolName, "mirror",
		cfg.Disks[0].StablePartitionPath(rootPartNum),
		cfg.Disks[1].StablePartitionPath(rootPartNum),
	)
}

func TestCreatePoolEnablesAutotrimOnFlashOnly(t *testing.T) {
	withMountRoot(t)
	flash := disks(2, 40)
	f := fakeCommands(t)
	if err := createPool(DiskConfig{FS: FSZFSRAID1, Disks: flash}, ZFSOpts{Ashift: 12, Copies: 1}); err != nil {
		t.Fatalf("createPool: %v", err)
	}
	f.mustRun(t, "zpool set autotrim=on "+ZFSPoolName)

	spinning := disks(2, 40)
	spinning[1].Rotational = true
	f2 := fakeCommands(t)
	if err := createPool(DiskConfig{FS: FSZFSRAID1, Disks: spinning}, ZFSOpts{Ashift: 12, Copies: 1}); err != nil {
		t.Fatalf("createPool: %v", err)
	}
	f2.mustNotRun(t, "autotrim")
}

func TestCreatePoolWrapsTheZpoolFailure(t *testing.T) {
	withMountRoot(t)
	f := fakeCommands(t)
	f.fail = func(name string, args []string) error {
		if name == "zpool" && len(args) > 0 && args[0] == "create" {
			return errors.New("device is in use")
		}
		return nil
	}
	err := createPool(DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)}, ZFSOpts{Ashift: 12, Copies: 1})
	if err == nil || !strings.Contains(err.Error(), "zpool create") {
		t.Fatalf("err = %v, want a zpool create failure", err)
	}
}

func TestCreatePoolRejectsAnUnbuildableTopology(t *testing.T) {
	withMountRoot(t)
	fakeCommands(t)
	if err := createPool(DiskConfig{FS: FSZFSRAID1, Disks: disks(1, 40)}, ZFSOpts{Ashift: 12}); err == nil {
		t.Fatal("expected a vdev error for a one-disk mirror")
	}
}

func TestCreateDatasetsCreatesEveryDataset(t *testing.T) {
	f := fakeCommands(t)
	if err := createDatasets(); err != nil {
		t.Fatalf("createDatasets: %v", err)
	}
	for _, ds := range datasets {
		f.mustRun(t, "zfs create", "mountpoint="+ds.mountpoint, ds.name)
	}
}

func TestCreateDatasetsNamesTheDatasetThatFailed(t *testing.T) {
	f := fakeCommands(t)
	f.fail = func(name string, _ []string) error {
		if name == "zfs" {
			return errors.New("out of space")
		}
		return nil
	}
	err := createDatasets()
	if err == nil || !strings.Contains(err.Error(), datasets[0].name) {
		t.Fatalf("err = %v, want it to name %s", err, datasets[0].name)
	}
}

func TestExportPoolToleratesABusyPool(t *testing.T) {
	f := fakeCommands(t)
	f.fail = func(string, []string) error { return errors.New("pool is busy") }
	exportPool()
	f.mustRun(t, "zpool export "+ZFSPoolName)
}

func TestWriteZFSSystemConfigDeclaresTheARCCeilingToBothConsumers(t *testing.T) {
	root := withMountRoot(t)

	if err := writeZFSSystemConfig(ZFSOpts{ARCMaxMiB: 4096}); err != nil {
		t.Fatalf("writeZFSSystemConfig: %v", err)
	}

	modprobe := readFile(t, root, "etc/modprobe.d/zfs.conf")
	if !strings.Contains(modprobe, fmt.Sprintf("zfs_arc_max=%d", int64(4096)<<20)) {
		t.Errorf("zfs.conf does not cap the ARC in bytes:\n%s", modprobe)
	}

	// The daemon cannot see the ARC, so the reserve has to account for it or
	// guests get admitted into memory the ARC already owns.
	env := readFile(t, root, "etc/spinifex/host.env")
	want := fmt.Sprintf("SPINIFEX_RESERVED_MEM_GB=%.1f", defaultHostReserveGB+4.0)
	if !strings.Contains(env, want) {
		t.Errorf("host.env = %q, want it to contain %q", env, want)
	}
}

func TestWriteTargetFileAppliesTheModeOnRewrite(t *testing.T) {
	root := withMountRoot(t)

	if err := writeTargetFile("/usr/sbin/tool", []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatalf("writeTargetFile: %v", err)
	}
	// A retried install rewrites an existing file, and WriteFile would leave
	// the original mode in place.
	if err := writeTargetFile("/usr/sbin/tool", []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
		t.Fatalf("writeTargetFile rewrite: %v", err)
	}

	st, err := os.Stat(filepath.Join(root, "usr/sbin/tool"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755", st.Mode().Perm())
	}
	if got := readFile(t, root, "usr/sbin/tool"); !strings.Contains(got, "true") {
		t.Errorf("content not rewritten: %q", got)
	}
}

func TestWriteGrubDefaultsPinsTheZFSRoot(t *testing.T) {
	root := withMountRoot(t)
	targetSkeleton(t, root)

	if err := writeGrubDefaults(DiskConfig{FS: FSZFSRAID1}); err != nil {
		t.Fatalf("writeGrubDefaults: %v", err)
	}
	got := readFile(t, root, "etc/default/grub")
	if !strings.Contains(got, "root=ZFS="+ZFSRootDataset) || !strings.Contains(got, "boot=zfs") {
		t.Errorf("grub defaults do not name the ZFS root:\n%s", got)
	}
	// Serial is listed last so it wins as the system console on headless racks.
	if !strings.Contains(got, "console=tty0 console=ttyS0,115200") {
		t.Errorf("grub defaults do not set both consoles:\n%s", got)
	}
	if theme := readFile(t, root, "etc/grub.d/06_spinifex"); !strings.Contains(theme, "terminal_output gfxterm") {
		t.Errorf("branding snippet missing gfxterm:\n%s", theme)
	}
}

func TestWriteGrubDefaultsLeavesExt4RootToGrubProbe(t *testing.T) {
	root := withMountRoot(t)
	targetSkeleton(t, root)
	if err := writeGrubDefaults(DiskConfig{FS: FSExt4}); err != nil {
		t.Fatalf("writeGrubDefaults: %v", err)
	}
	if got := readFile(t, root, "etc/default/grub"); strings.Contains(got, "root=ZFS=") {
		t.Errorf("ext4 install should not pin a ZFS root:\n%s", got)
	}
}

func TestDivertGrubInstallReplacesTheBinary(t *testing.T) {
	root := withMountRoot(t)
	f := fakeCommands(t)

	if err := divertGrubInstall(); err != nil {
		t.Fatalf("divertGrubInstall: %v", err)
	}
	// The diversion is what makes the replacement survive a grub-pc upgrade.
	f.mustRun(t, "dpkg-divert --divert "+grubInstallReal, "--rename --add "+grubInstallPath)
	if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(grubInstallPath, "/"))); err != nil {
		t.Errorf("wrapper not written: %v", err)
	}
}

func TestDivertGrubInstallFailsLoudly(t *testing.T) {
	withMountRoot(t)
	f := fakeCommands(t)
	f.fail = func(string, []string) error { return errors.New("dpkg is locked") }

	if err := divertGrubInstall(); err == nil || !strings.Contains(err.Error(), "divert grub-install") {
		t.Fatalf("err = %v, want a diversion failure", err)
	}
}

// espFixture writes the state a successful boot-tool run leaves behind: the
// registered ESP list, and a grub.cfg on the ESP that verifyBootConfig mounts.
func espFixture(t *testing.T, root string, uuids string, grubCfg string) {
	t.Helper()
	list := filepath.Join(root, strings.TrimPrefix(bootToolUUIDList, "/"))
	if err := os.MkdirAll(filepath.Dir(list), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(list, []byte(uuids), 0o644); err != nil {
		t.Fatal(err)
	}
	if grubCfg == "" {
		return
	}
	dir := filepath.Join(root, "var/tmp/verify-esp/grub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "grub.cfg"), []byte(grubCfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBootConfig(t *testing.T) {
	goodCfg := "linux /vmlinuz-6.12.0 root=ZFS=" + ZFSRootDataset + " boot=zfs\n"

	tests := []struct {
		name    string
		uuids   string
		grubCfg string
		wantErr string
	}{
		{name: "every ESP registered and pointing at the pool", uuids: "AAAA-1111\nBBBB-2222\n", grubCfg: goodCfg},
		{
			name:    "a disk got no bootloader",
			uuids:   "AAAA-1111\n",
			grubCfg: goodCfg,
			wantErr: "expected 2 registered ESPs, found 1",
		},
		{
			// grub-mkconfig degrades quietly when grub-probe cannot read the
			// pool, and the failure only shows up as a panic at boot.
			name:    "grub.cfg names no root",
			uuids:   "AAAA-1111\nBBBB-2222\n",
			grubCfg: "linux /vmlinuz-6.12.0 root=/dev/sda3\n",
			wantErr: "would not find its root filesystem",
		},
		{
			name:    "grub.cfg names no kernel",
			uuids:   "AAAA-1111\nBBBB-2222\n",
			grubCfg: "set root=ZFS=" + ZFSRootDataset + "\n",
			wantErr: "references no kernel",
		},
		{name: "no ESP list at all", wantErr: "read ESP list"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := withMountRoot(t)
			fakeCommands(t)
			cfg := DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)}
			if tt.uuids != "" {
				espFixture(t, root, tt.uuids, tt.grubCfg)
			}

			err := verifyBootConfig(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyBootConfig: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestInstallBootToolZFSSetsUpEveryESP(t *testing.T) {
	root := withMountRoot(t)
	f := fakeCommands(t)
	cfg := DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)}
	targetSkeleton(t, root)
	espFixture(t, root, "AAAA-1111\nBBBB-2222\n", "linux /vmlinuz-6.12.0 root=ZFS="+ZFSRootDataset+"\n")

	if err := installBootToolZFS(cfg); err != nil {
		t.Fatalf("installBootToolZFS: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(bootToolPath, "/"))); err != nil {
		t.Errorf("boot tool not installed: %v", err)
	}
	// A partially-updated set of ESPs boots today and fails on the next kernel
	// upgrade, so every disk is initialised under strict mode.
	for _, d := range cfg.Disks {
		f.mustRun(t, "SPINIFEX_BOOT_TOOL_STRICT=1", "init", d.PartitionPath(espPartNum))
	}
	// NVRAM is written exactly once, by the initial refresh.
	f.mustRun(t, "SPINIFEX_BOOT_TOOL_NVRAM=1", "refresh")
	f.mustRun(t, "dpkg-divert")
	for _, m := range chrootMountPaths {
		f.mustRun(t, "mount --bind /"+m)
	}
}

func TestInstallBootToolZFSStopsWhenADiskFails(t *testing.T) {
	root := withMountRoot(t)
	f := fakeCommands(t)
	cfg := DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)}
	targetSkeleton(t, root)
	espFixture(t, root, "AAAA-1111\nBBBB-2222\n", "linux /vmlinuz root=ZFS="+ZFSRootDataset+"\n")
	f.fail = func(name string, args []string) error {
		if strings.Contains(strings.Join(args, " "), "init") {
			return errors.New("no ESP")
		}
		return nil
	}

	err := installBootToolZFS(cfg)
	if err == nil || !strings.Contains(err.Error(), "spinifex-boot-tool init") {
		t.Fatalf("err = %v, want the init failure", err)
	}
	f.mustNotRun(t, "SPINIFEX_BOOT_TOOL_NVRAM=1")
}

func TestRefreshBootToolNoteOnlyAppliesToZFS(t *testing.T) {
	// Log-only, but it is the operator's only record of the disk-replacement
	// command, so both branches are walked.
	refreshBootToolNote(DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)})
	refreshBootToolNote(DiskConfig{FS: FSExt4, Disks: disks(1, 40)})
}

func TestMaskSystemdUnitIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := maskSystemdUnit(root, "getty@tty1.service"); err != nil {
			t.Fatalf("maskSystemdUnit: %v", err)
		}
	}
	target, err := os.Readlink(filepath.Join(root, "etc/systemd/system/getty@tty1.service"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "/dev/null" {
		t.Errorf("mask target = %q, want /dev/null", target)
	}
}

func TestCopyAssetsAreBestEffort(t *testing.T) {
	root := t.TempDir()
	// Neither asset exists outside the live ISO; a missing one must not fail
	// the install, only the branding.
	copyGrubFont(root)
	copySplashImage(root)
}

func TestWriteFstabOnZFSHoldsNothingButSwap(t *testing.T) {
	root := withMountRoot(t)
	targetSkeleton(t, root)

	if err := writeFstab(DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)}); err != nil {
		t.Fatalf("writeFstab: %v", err)
	}
	got := readFile(t, root, "etc/fstab")
	// Root and every dataset are mounted from pool properties, and the ESPs on
	// demand by the boot tool.
	if strings.Contains(got, " / ") || strings.Contains(got, "/boot/efi") {
		t.Errorf("ZFS fstab should carry no root or ESP line:\n%s", got)
	}
}

func TestWriteNetworkConfigWritesTheWholeNetworkdTree(t *testing.T) {
	root := withMountRoot(t)
	targetSkeleton(t, root)

	if err := writeNetworkConfig(threeNIC()); err != nil {
		t.Fatalf("writeNetworkConfig: %v", err)
	}

	netd := filepath.Join(root, "etc/systemd/network")
	for _, plane := range []Plane{PlaneWAN, PlaneLAN, PlaneVPC} {
		if got := readFile(t, netd, fmt.Sprintf("20-spinifex-%s.netdev", plane)); !strings.Contains(got, "Kind=bridge") {
			t.Errorf("%s netdev is not a bridge:\n%s", plane, got)
		}
	}
	// Only br-wan may block network-online.target; the east-west bridges are
	// brought up by their own unit afterwards.
	if got := readFile(t, netd, "20-spinifex-wan.network"); strings.Contains(got, "ActivationPolicy=manual") {
		t.Errorf("br-wan must auto-activate:\n%s", got)
	}
	if got := readFile(t, netd, "20-spinifex-lan.network"); !strings.Contains(got, "ActivationPolicy=manual") {
		t.Errorf("br-lan must not auto-activate:\n%s", got)
	}

	// The live ISO masks wait-online; the mask is copied in by rsync and has to
	// be lifted, then scoped to br-wan so br-lan never blocks the boot.
	dropin := readFile(t, root, "etc/systemd/system/systemd-networkd-wait-online.service.d/spinifex-wan-only.conf")
	if !strings.Contains(dropin, "--interface=br-wan") || !strings.Contains(dropin, "--timeout=60") {
		t.Errorf("wait-online drop-in is not scoped to br-wan:\n%s", dropin)
	}

	sysctl := readFile(t, root, "etc/sysctl.d/99-spinifex-network.conf")
	for _, br := range []string{"br-wan", "br-lan", "br-vpc"} {
		if !strings.Contains(sysctl, "net.ipv6.conf."+br+".disable_ipv6=1") {
			t.Errorf("IPv6 not disabled on %s:\n%s", br, sysctl)
		}
	}
}

func TestWriteNetworkConfigRejectsAnInvalidRoleSet(t *testing.T) {
	root := withMountRoot(t)
	targetSkeleton(t, root)
	cfg := threeNIC()
	cfg.LAN.Interface = cfg.WAN.Interface

	if err := writeNetworkConfig(cfg); err == nil || !strings.Contains(err.Error(), "network roles") {
		t.Fatalf("err = %v, want the role validation to reject two planes on one link", err)
	}
}

func TestWriteNetworkConfigLiftsTheWaitOnlineMask(t *testing.T) {
	root := withMountRoot(t)
	mask := filepath.Join(root, "etc/systemd/system/systemd-networkd-wait-online.service")
	if err := os.MkdirAll(filepath.Dir(mask), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", mask); err != nil {
		t.Fatal(err)
	}

	if err := writeNetworkConfig(threeNIC()); err != nil {
		t.Fatalf("writeNetworkConfig: %v", err)
	}
	if _, err := os.Lstat(mask); err == nil {
		t.Error("the wait-online mask survived; firstboot would run before br-wan has a lease")
	}
}

func TestBindChrootMountsCreatesEveryMountpoint(t *testing.T) {
	root := withMountRoot(t)
	f := fakeCommands(t)

	if err := bindChrootMounts(); err != nil {
		t.Fatalf("bindChrootMounts: %v", err)
	}
	for _, m := range chrootMountPaths {
		if _, err := os.Stat(filepath.Join(root, m)); err != nil {
			t.Errorf("mountpoint /%s not created: %v", m, err)
		}
		f.mustRun(t, "mount --bind /"+m+" "+filepath.Join(root, m))
	}

	unbindChrootMounts()
	// Reverse order: /sys and /proc come out before /dev they were nested under.
	if f.indexOf("umount "+filepath.Join(root, "sys")) > f.indexOf("umount "+filepath.Join(root, "dev")) {
		t.Error("chroot mounts were unbound in bind order, not reverse")
	}
}

func TestBindChrootMountsReportsAFailedBind(t *testing.T) {
	withMountRoot(t)
	f := fakeCommands(t)
	f.fail = func(name string, _ []string) error {
		if name == "mount" {
			return errors.New("permission denied")
		}
		return nil
	}
	if err := bindChrootMounts(); err == nil || !strings.Contains(err.Error(), "bind-mount") {
		t.Fatalf("err = %v, want a bind-mount failure", err)
	}
}
