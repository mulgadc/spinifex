package install

import (
	"slices"
	"strings"
	"testing"
)

// disk builds a test disk with a by-id path, since that is what pool members
// are actually named by.
func disk(name string, gib int64) Disk {
	return Disk{
		Path:              "/dev/" + name,
		ByID:              "/dev/disk/by-id/virtio-" + name,
		Bytes:             gib << 30,
		LogicalBlockSize:  512,
		PhysicalBlockSize: 512,
	}
}

func disks(n int, gib int64) []Disk {
	out := make([]Disk, n)
	for i := range out {
		out[i] = disk(string(rune('a'+i)), gib)
	}
	return out
}

func TestBuildVdev(t *testing.T) {
	tests := []struct {
		name  string
		fs    FSType
		disks []Disk
		want  string
	}{
		{
			"raid0 is a bare stripe with no keyword",
			FSZFSRAID0, disks(2, 40),
			"/dev/disk/by-id/virtio-a-part3 /dev/disk/by-id/virtio-b-part3",
		},
		{
			"single-disk raid0 is the one-drive case",
			FSZFSRAID0, disks(1, 40),
			"/dev/disk/by-id/virtio-a-part3",
		},
		{
			"raid1 is one mirror vdev",
			FSZFSRAID1, disks(2, 40),
			"mirror /dev/disk/by-id/virtio-a-part3 /dev/disk/by-id/virtio-b-part3",
		},
		{
			"raid1 across three disks is a three-way mirror, not a stripe",
			FSZFSRAID1, disks(3, 40),
			"mirror /dev/disk/by-id/virtio-a-part3 /dev/disk/by-id/virtio-b-part3 /dev/disk/by-id/virtio-c-part3",
		},
		{
			"raid10 pairs members in selection order",
			FSZFSRAID10, disks(4, 40),
			"mirror /dev/disk/by-id/virtio-a-part3 /dev/disk/by-id/virtio-b-part3 " +
				"mirror /dev/disk/by-id/virtio-c-part3 /dev/disk/by-id/virtio-d-part3",
		},
		{
			"raidz1 across three disks",
			FSZFSRAIDZ1, disks(3, 40),
			"raidz1 /dev/disk/by-id/virtio-a-part3 /dev/disk/by-id/virtio-b-part3 /dev/disk/by-id/virtio-c-part3",
		},
		{
			"raidz2 needs four",
			FSZFSRAIDZ2, disks(4, 40),
			"raidz2 /dev/disk/by-id/virtio-a-part3 /dev/disk/by-id/virtio-b-part3 " +
				"/dev/disk/by-id/virtio-c-part3 /dev/disk/by-id/virtio-d-part3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildVdev(DiskConfig{FS: tt.fs, Disks: tt.disks})
			if err != nil {
				t.Fatalf("buildVdev: %v", err)
			}
			if got != tt.want {
				t.Errorf("buildVdev()\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

// A pool built on /dev/sdX silently breaks when the controller enumerates in a
// different order, so the fallback only applies when there is no by-id path.
func TestBuildVdevFallsBackToKernelNameWithoutByID(t *testing.T) {
	d := disk("a", 40)
	d.ByID = ""
	got, err := buildVdev(DiskConfig{FS: FSZFSRAID0, Disks: []Disk{d}})
	if err != nil {
		t.Fatalf("buildVdev: %v", err)
	}
	if got != "/dev/a3" {
		t.Errorf("buildVdev() = %q, want /dev/a3", got)
	}
}

func TestBuildVdevRejectsExt4(t *testing.T) {
	if _, err := buildVdev(DiskConfig{FS: FSExt4, Disks: disks(1, 40)}); err == nil {
		t.Fatal("buildVdev accepted ext4")
	}
}

func TestBuildVdevValidatesFirst(t *testing.T) {
	// Three disks cannot form striped mirrors, and producing a two-disk mirror
	// plus a silently dropped disk would be worse than an error.
	if _, err := buildVdev(DiskConfig{FS: FSZFSRAID10, Disks: disks(3, 40)}); err == nil {
		t.Fatal("buildVdev accepted an odd disk count for RAID10")
	}
}

func TestDetectAshift(t *testing.T) {
	tests := []struct {
		name     string
		logical  int
		physical int
		want     int
	}{
		// ashift cannot be changed after creation and one that is too small
		// costs a read-modify-write per block forever, so 512e drives are
		// deliberately treated as 4K.
		{"512n drive is floored at 4K", 512, 512, 12},
		{"512e drive reports 4K physical", 512, 4096, 12},
		{"4Kn drive", 4096, 4096, 12},
		{"8K native drive", 8192, 8192, 13},
		{"16K native drive", 16384, 16384, 14},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Disk{LogicalBlockSize: tt.logical, PhysicalBlockSize: tt.physical}
			if got := detectAshift([]Disk{d}); got != tt.want {
				t.Errorf("detectAshift() = %d, want %d", got, tt.want)
			}
		})
	}
}

// A pool cannot have two sector sizes, so the widest member wins — the reverse
// would make every write to the large-sector disk a read-modify-write.
func TestDetectAshiftTakesWidestMember(t *testing.T) {
	got := detectAshift([]Disk{
		{LogicalBlockSize: 512, PhysicalBlockSize: 512},
		{LogicalBlockSize: 512, PhysicalBlockSize: 8192},
	})
	if got != 13 {
		t.Errorf("detectAshift() = %d, want 13", got)
	}
}

func TestDefaultARCMaxMiB(t *testing.T) {
	tests := []struct {
		name    string
		totalMi int
		want    int
	}{
		{"tiny VM gets 10% of very little", 2048, 205},
		{"8GB node gets 10%", 8192, 819},
		{"64GB node gets 10%", 65536, 6554},
		// The point of the cap: guest RAM is what this machine sells, and half
		// of a 256GB node is 128GB of instance capacity the ARC would swallow.
		{"256GB node is capped at 16GiB", 262144, 16384},
		{"1TB node is still capped at 16GiB", 1048576, 16384},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultARCMaxMiB(tt.totalMi); got != tt.want {
				t.Errorf("defaultARCMaxMiB(%d) = %d, want %d", tt.totalMi, got, tt.want)
			}
		})
	}
}

// An unreadable /proc/meminfo must not produce a zero or negative ceiling,
// which ZFS would either reject or read as "no limit".
func TestDefaultARCMaxMiBNeverGoesBelowFloor(t *testing.T) {
	for _, total := range []int{0, 128, 512} {
		if got := defaultARCMaxMiB(total); got < arcMinMiB {
			t.Errorf("defaultARCMaxMiB(%d) = %d, below the floor of %d", total, got, arcMinMiB)
		}
	}
}

func TestResolveZFSOptsFillsDefaults(t *testing.T) {
	got := resolveZFSOpts(DiskConfig{FS: FSZFSRAIDZ1, Disks: disks(3, 40)})
	if got.Ashift != 12 {
		t.Errorf("Ashift = %d, want 12", got.Ashift)
	}
	if got.Compress != "lz4" {
		t.Errorf("Compress = %q, want lz4", got.Compress)
	}
	if got.Checksum != "on" {
		t.Errorf("Checksum = %q, want on", got.Checksum)
	}
	if got.Copies != 1 {
		t.Errorf("Copies = %d, want 1", got.Copies)
	}
	if got.ARCMaxMiB <= 0 {
		t.Errorf("ARCMaxMiB = %d, want a positive ceiling", got.ARCMaxMiB)
	}
}

func TestResolveZFSOptsKeepsOperatorChoices(t *testing.T) {
	in := DiskConfig{
		FS:    FSZFSRAIDZ1,
		Disks: disks(3, 40),
		ZFS:   ZFSOpts{Ashift: 13, Compress: "zstd", Checksum: "blake3", Copies: 2, ARCMaxMiB: 2048},
	}
	got := resolveZFSOpts(in)
	if got != in.ZFS {
		t.Errorf("resolveZFSOpts overrode explicit options: got %+v, want %+v", got, in.ZFS)
	}
}

func TestParseImportablePools(t *testing.T) {
	// Captured from `zpool import` on a machine with a pool from a previous
	// install: the config tree below each header must not be mistaken for one.
	const out = `   pool: rpool
     id: 8543219876543210987
  state: ONLINE
 action: The pool can be imported using its name or numeric identifier.
 config:

	rpool                     ONLINE
	  raidz1-0                ONLINE
	    virtio-spinifex-disk1-part3  ONLINE
	    virtio-spinifex-disk2-part3  ONLINE

   pool: tank
     id: 1111111111111111111
  state: ONLINE
 config:

	tank                      ONLINE
`
	got := parseImportablePools(out)
	want := []string{"rpool", "tank"}
	if len(got) != len(want) {
		t.Fatalf("parseImportablePools() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pool %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseImportablePoolsEmpty(t *testing.T) {
	if got := parseImportablePools("no pools available to import\n"); len(got) != 0 {
		t.Errorf("parseImportablePools() = %v, want none", got)
	}
}

// Every dataset must be creatable in order: a child cannot be created before
// its parent, and getting this wrong only shows up on real hardware.
func TestDatasetsAreOrderedParentFirst(t *testing.T) {
	seen := map[string]bool{ZFSPoolName: true}
	for _, ds := range datasets {
		parent := ds.name[:strings.LastIndex(ds.name, "/")]
		if !seen[parent] {
			t.Errorf("dataset %s is created before its parent %s", ds.name, parent)
		}
		seen[ds.name] = true
	}
}

// The root dataset has to be the one the bootloader names, or the machine
// mounts an empty filesystem and panics.
func TestRootDatasetIsCreated(t *testing.T) {
	for _, ds := range datasets {
		if ds.name == ZFSRootDataset {
			if ds.mountpoint != "/" {
				t.Fatalf("%s mountpoint = %q, want /", ZFSRootDataset, ds.mountpoint)
			}
			return
		}
	}
	t.Fatalf("%s is never created", ZFSRootDataset)
}

// rsync --delete tries to rmdir a dataset mountpoint the live source does not
// have, which fails on a busy mount and aborts the whole copy. Every dataset
// with a real mountpoint must be protected.
func TestDatasetProtectFilters(t *testing.T) {
	got := datasetProtectFilters(DiskConfig{FS: FSZFSRAIDZ1, Disks: disks(3, 40)})
	for _, ds := range datasets {
		if ds.mountpoint == "/" || ds.mountpoint == "none" {
			continue
		}
		want := "--filter=P " + ds.mountpoint
		if !slices.Contains(got, want) {
			t.Errorf("missing protect rule for %s (%s)", ds.name, ds.mountpoint)
		}
	}
	// The root dataset is the destination itself, and "none" datasets are never
	// mounted, so protecting either would be meaningless.
	for _, f := range got {
		if f == "--filter=P /" || f == "--filter=P none" {
			t.Errorf("unexpected protect rule %q", f)
		}
	}
}

func TestDatasetProtectFiltersSkipsExt4(t *testing.T) {
	if got := datasetProtectFilters(DiskConfig{FS: FSExt4, Disks: disks(1, 40)}); got != nil {
		t.Errorf("datasetProtectFilters() = %v on ext4, want none", got)
	}
}
