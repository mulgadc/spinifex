package install

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// fakeDisk describes one device to lay down in the fixture sysfs tree.
type fakeDisk struct {
	name       string
	sectors    int64
	model      string
	logical    int
	physical   int
	rotational bool
	removable  bool
	partitions []string
}

// fakeSysfs builds a /sys/block, /sys/class/block, /dev/disk/by-id and
// /proc/mounts fixture and points the scan at it for the duration of the test.
type fakeSysfs struct {
	root string
	t    *testing.T
}

func newFakeSysfs(t *testing.T, disks ...fakeDisk) *fakeSysfs {
	t.Helper()
	f := &fakeSysfs{root: t.TempDir(), t: t}

	swap := func(dst *string, v string) {
		prev := *dst
		*dst = v
		t.Cleanup(func() { *dst = prev })
	}
	swap(&sysBlockDir, filepath.Join(f.root, "sys/block"))
	swap(&sysClassBlockDir, filepath.Join(f.root, "sys/class/block"))
	swap(&devByIDDir, filepath.Join(f.root, "dev/disk/by-id"))
	swap(&procMountsPath, filepath.Join(f.root, "proc/mounts"))

	f.mkdirAll(sysBlockDir)
	f.mkdirAll(sysClassBlockDir)
	for _, d := range disks {
		f.addDisk(d)
	}
	return f
}

func (f *fakeSysfs) mkdirAll(dir string) {
	f.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func (f *fakeSysfs) write(path, content string) {
	f.t.Helper()
	f.mkdirAll(filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
}

// addDisk lays out one device the way the kernel does: the attribute files
// under /sys/block/<dev>, and a /sys/class/block symlink per disk and
// partition, since parentDisk walks the latter.
func (f *fakeSysfs) addDisk(d fakeDisk) {
	f.t.Helper()
	dir := filepath.Join(sysBlockDir, d.name)
	f.write(filepath.Join(dir, "size"), strconv.FormatInt(d.sectors, 10)+"\n")
	if d.model != "" {
		f.write(filepath.Join(dir, "device/model"), d.model+"\n")
	}
	if d.logical > 0 {
		f.write(filepath.Join(dir, "queue/logical_block_size"), strconv.Itoa(d.logical)+"\n")
	}
	if d.physical > 0 {
		f.write(filepath.Join(dir, "queue/physical_block_size"), strconv.Itoa(d.physical)+"\n")
	}
	f.write(filepath.Join(dir, "queue/rotational"), boolAttr(d.rotational))
	f.write(filepath.Join(dir, "removable"), boolAttr(d.removable))

	f.symlink(dir, filepath.Join(sysClassBlockDir, d.name))
	for _, part := range d.partitions {
		partDir := filepath.Join(dir, part)
		f.write(filepath.Join(partDir, "partition"), "1\n")
		f.symlink(partDir, filepath.Join(sysClassBlockDir, part))
	}
}

func (f *fakeSysfs) symlink(target, link string) {
	f.t.Helper()
	f.mkdirAll(filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		f.t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

// addByID creates a by-id alias pointing at a kernel device name. The link
// target must exist for EvalSymlinks, so a placeholder /dev tree is used.
func (f *fakeSysfs) addByID(id, dev string) {
	f.t.Helper()
	devPath := filepath.Join(f.root, "dev", dev)
	f.write(devPath, "")
	f.symlink(devPath, filepath.Join(devByIDDir, id))
}

func (f *fakeSysfs) setMounts(content string) {
	f.t.Helper()
	f.write(procMountsPath, content)
}

func boolAttr(b bool) string {
	if b {
		return "1\n"
	}
	return "0\n"
}

func TestListDisksReadsSysfsAttributes(t *testing.T) {
	f := newFakeSysfs(t, fakeDisk{
		name: "sdb", sectors: 2 << 20, model: "SAMSUNG MZ7", logical: 512, physical: 4096, rotational: true,
	})
	f.setMounts("")

	disks, err := ListDisks()
	if err != nil {
		t.Fatalf("ListDisks: %v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("got %d disks, want 1: %+v", len(disks), disks)
	}
	d := disks[0]
	if d.Path != "/dev/sdb" || d.Bytes != (2<<20)*512 {
		t.Errorf("path/size = %s/%d", d.Path, d.Bytes)
	}
	if d.Model != "SAMSUNG MZ7" {
		t.Errorf("Model = %q", d.Model)
	}
	if d.LogicalBlockSize != 512 || d.PhysicalBlockSize != 4096 {
		t.Errorf("block sizes = %d/%d", d.LogicalBlockSize, d.PhysicalBlockSize)
	}
	if !d.Rotational || d.Removable || d.LiveMedia {
		t.Errorf("flags = rot %v, rem %v, live %v", d.Rotational, d.Removable, d.LiveMedia)
	}
	if d.Content == "" {
		t.Error("Content is empty; an unreadable device must report something")
	}
}

func TestListDisksSkipsVirtualAndEmptyDevices(t *testing.T) {
	f := newFakeSysfs(t,
		fakeDisk{name: "sda", sectors: 1 << 20},
		fakeDisk{name: "loop0", sectors: 1 << 20},
		fakeDisk{name: "dm-0", sectors: 1 << 20},
		fakeDisk{name: "zram0", sectors: 1 << 20},
		fakeDisk{name: "sr0", sectors: 1 << 20},
		fakeDisk{name: "nbd0", sectors: 1 << 20},
		// An empty card-reader slot reports zero sectors.
		fakeDisk{name: "mmcblk0", sectors: 0},
	)
	f.setMounts("")

	disks, err := ListDisks()
	if err != nil {
		t.Fatalf("ListDisks: %v", err)
	}
	var paths []string
	for _, d := range disks {
		paths = append(paths, d.Path)
	}
	if !slices.Equal(paths, []string{"/dev/sda"}) {
		t.Errorf("paths = %v, want only /dev/sda", paths)
	}
}

func TestListDisksSortsByPath(t *testing.T) {
	f := newFakeSysfs(t,
		fakeDisk{name: "sdc", sectors: 1 << 20},
		fakeDisk{name: "sda", sectors: 1 << 20},
		fakeDisk{name: "sdb", sectors: 1 << 20},
	)
	f.setMounts("")

	disks, err := ListDisks()
	if err != nil {
		t.Fatalf("ListDisks: %v", err)
	}
	var paths []string
	for _, d := range disks {
		paths = append(paths, d.Path)
	}
	if !slices.Equal(paths, []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}) {
		t.Errorf("paths = %v, want sorted", paths)
	}
}

func TestListDisksDefaultsBlockSizesWhenSysfsIsSilent(t *testing.T) {
	f := newFakeSysfs(t, fakeDisk{name: "vda", sectors: 1 << 20})
	f.setMounts("")

	disks, err := ListDisks()
	if err != nil {
		t.Fatalf("ListDisks: %v", err)
	}
	if disks[0].LogicalBlockSize != 512 || disks[0].PhysicalBlockSize != 512 {
		t.Errorf("block sizes = %d/%d, want the 512 default",
			disks[0].LogicalBlockSize, disks[0].PhysicalBlockSize)
	}
}

func TestListDisksErrorsWhenSysfsIsUnreadable(t *testing.T) {
	newFakeSysfs(t)
	sysBlockDir = filepath.Join(t.TempDir(), "does-not-exist")

	if _, err := ListDisks(); err == nil {
		t.Fatal("expected an error when /sys/block cannot be read")
	}
}

func TestListDisksMarksTheLiveMedium(t *testing.T) {
	f := newFakeSysfs(t,
		fakeDisk{name: "sda", sectors: 1 << 20},
		fakeDisk{name: "sdz", sectors: 1 << 20, removable: true, partitions: []string{"sdz1"}},
	)
	// The installer boots from a partition; the whole disk must be excluded.
	f.setMounts("/dev/sdz1 /run/live/medium iso9660 ro 0 0\n" +
		"proc /proc proc rw 0 0\n" +
		"/dev/sda1 / ext4 rw 0 0\n")

	disks, err := ListDisks()
	if err != nil {
		t.Fatalf("ListDisks: %v", err)
	}
	byPath := map[string]Disk{}
	for _, d := range disks {
		byPath[d.Path] = d
	}
	if !byPath["/dev/sdz"].LiveMedia {
		t.Error("/dev/sdz backs the live medium and was not marked")
	}
	if byPath["/dev/sda"].LiveMedia {
		t.Error("/dev/sda is not the live medium but was marked")
	}
}

func TestLiveMediaDevicesIsEmptyWithoutProcMounts(t *testing.T) {
	newFakeSysfs(t)
	if got := liveMediaDevices(); len(got) != 0 {
		t.Errorf("liveMediaDevices = %v, want empty when /proc/mounts is missing", got)
	}
}

func TestParentDiskLeavesWholeDisksAlone(t *testing.T) {
	newFakeSysfs(t, fakeDisk{name: "sda", sectors: 1 << 20, partitions: []string{"sda1"}})

	if got := parentDisk("sda"); got != "sda" {
		t.Errorf("parentDisk(sda) = %q, want sda", got)
	}
	if got := parentDisk("sda1"); got != "sda" {
		t.Errorf("parentDisk(sda1) = %q, want sda", got)
	}
	// An unknown name cannot be resolved and is returned unchanged.
	if got := parentDisk("sdq9"); got != "sdq9" {
		t.Errorf("parentDisk(sdq9) = %q, want it unchanged", got)
	}
}

func TestBuildByIDIndexPrefersTheMostLegibleAlias(t *testing.T) {
	f := newFakeSysfs(t, fakeDisk{name: "sda", sectors: 1 << 20})
	// wwn- is stable but opaque, so ata- must win regardless of scan order.
	f.addByID("wwn-0x5000c500", "sda")
	f.addByID("ata-SAMSUNG_MZ7_S1", "sda")
	f.addByID("ata-SAMSUNG_MZ7_S1-part1", "sda")

	idx := buildByIDIndex()
	want := filepath.Join(devByIDDir, "ata-SAMSUNG_MZ7_S1")
	if idx["sda"] != want {
		t.Errorf("by-id[sda] = %q, want %q", idx["sda"], want)
	}
}

func TestBuildByIDIndexIsEmptyWhenTheDirIsAbsent(t *testing.T) {
	newFakeSysfs(t)
	if got := buildByIDIndex(); len(got) != 0 {
		t.Errorf("buildByIDIndex = %v, want empty", got)
	}
}

func TestListDisksAttachesByIDPath(t *testing.T) {
	f := newFakeSysfs(t, fakeDisk{name: "sda", sectors: 1 << 20})
	f.addByID("ata-TEST_SERIAL0", "sda")
	f.setMounts("")

	disks, err := ListDisks()
	if err != nil {
		t.Fatalf("ListDisks: %v", err)
	}
	want := filepath.Join(devByIDDir, "ata-TEST_SERIAL0")
	if disks[0].ByID != want {
		t.Errorf("ByID = %q, want %q", disks[0].ByID, want)
	}
	if disks[0].Stable() != want {
		t.Errorf("Stable() = %q, want the by-id path", disks[0].Stable())
	}
}

func TestPrefixRank(t *testing.T) {
	if prefixRank("nvme-Foo") >= prefixRank("wwn-0x1") {
		t.Error("nvme- must outrank wwn-")
	}
	if prefixRank("md-uuid-x") != len(byIDPreference) {
		t.Error("an unknown namespace must rank last")
	}
}

func TestSizeHuman(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{2 << 40, "2.0T"},
		{500 << 30, "500.0G"},
		{64 << 20, "64.0M"},
		{512, "512B"},
	}
	for _, tt := range tests {
		if got := (Disk{Bytes: tt.bytes}).SizeHuman(); got != tt.want {
			t.Errorf("SizeHuman(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestStableFallsBackToKernelName(t *testing.T) {
	d := Disk{Path: "/dev/sda"}
	if d.Stable() != "/dev/sda" {
		t.Errorf("Stable() = %q", d.Stable())
	}
	if d.StablePartitionPath(2) != "/dev/sda2" {
		t.Errorf("StablePartitionPath(2) = %q", d.StablePartitionPath(2))
	}
}

func TestHasAnyPrefix(t *testing.T) {
	if !hasAnyPrefix("loop3", skipPrefixes) {
		t.Error("loop3 should match")
	}
	if hasAnyPrefix("sda", skipPrefixes) {
		t.Error("sda should not match")
	}
}
