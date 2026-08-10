package install

import (
	"strings"
	"testing"
)

func TestDiskConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     DiskConfig
		wantErr string
	}{
		{"ext4 on one disk", DiskConfig{FS: FSExt4, Disks: disks(1, 40)}, ""},
		{"raidz1 on three matching disks", DiskConfig{FS: FSZFSRAIDZ1, Disks: disks(3, 40)}, ""},
		{"raid10 on four matching disks", DiskConfig{FS: FSZFSRAID10, Disks: disks(4, 40)}, ""},
		// The single-drive ZFS case: no redundancy, but checksums, compression
		// and snapshots still apply.
		{"raid0 on one disk", DiskConfig{FS: FSZFSRAID0, Disks: disks(1, 40)}, ""},

		{"no filesystem chosen", DiskConfig{Disks: disks(1, 40)}, "no filesystem selected"},
		{"no disks", DiskConfig{FS: FSZFSRAIDZ1}, "select at least one disk"},
		{"ext4 spans disks only with roles", DiskConfig{FS: FSExt4, Disks: disks(2, 40)}, "no roles assigned"},
		{"raidz1 needs three", DiskConfig{FS: FSZFSRAIDZ1, Disks: disks(2, 40)}, "at least 3 disks"},
		{"raidz2 needs four", DiskConfig{FS: FSZFSRAIDZ2, Disks: disks(3, 40)}, "at least 4 disks"},
		{"raidz3 needs five", DiskConfig{FS: FSZFSRAIDZ3, Disks: disks(4, 40)}, "at least 5 disks"},
		{"raid1 needs two", DiskConfig{FS: FSZFSRAID1, Disks: disks(1, 40)}, "at least 2 disks"},
		{"raid10 rejects an odd count", DiskConfig{FS: FSZFSRAID10, Disks: disks(5, 40)}, "even number"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateRejectsDuplicateDisk(t *testing.T) {
	d := disk("a", 40)
	err := DiskConfig{FS: FSZFSRAID1, Disks: []Disk{d, d}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("Validate() = %v, want a duplicate-selection error", err)
	}
}

func TestValidateRejectsLiveMedia(t *testing.T) {
	d := disk("a", 40)
	d.LiveMedia = true
	err := DiskConfig{FS: FSExt4, Disks: []Disk{d}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "boot media") {
		t.Fatalf("Validate() = %v, want the installer's own media to be refused", err)
	}
}

func TestValidateRejectsTooSmallDisk(t *testing.T) {
	err := DiskConfig{FS: FSExt4, Disks: []Disk{disk("a", 1)}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "too small") {
		t.Fatalf("Validate() = %v, want a minimum-size error", err)
	}
}

// A pool truncates every member to the smallest, so mismatched disks in a
// redundant topology silently throw capacity away.
func TestValidateSizeTolerance(t *testing.T) {
	tests := []struct {
		name    string
		fs      FSType
		sizes   []int64
		wantErr bool
	}{
		{"identical disks", FSZFSRAIDZ1, []int64{40, 40, 40}, false},
		{"within 10%", FSZFSRAIDZ1, []int64{100, 95, 92}, false},
		{"beyond 10%", FSZFSRAIDZ1, []int64{100, 100, 80}, true},
		{"mirror with a mismatched member", FSZFSRAID1, []int64{100, 50}, true},
		// A stripe has no parity to waste, so mismatched sizes are a warning
		// rather than a refusal — matching Proxmox.
		{"raid0 permits mismatched disks", FSZFSRAID0, []int64{100, 40}, false},
		{"raid10 checks each pair, not the whole pool", FSZFSRAID10, []int64{40, 40, 100, 100}, false},
		{"raid10 rejects a mismatched pair", FSZFSRAID10, []int64{40, 100, 100, 100}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			members := make([]Disk, len(tt.sizes))
			for i, gib := range tt.sizes {
				members[i] = disk(string(rune('a'+i)), gib)
			}
			err := DiskConfig{FS: tt.fs, Disks: members}.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want a size-mismatch error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestWarningsFlagMismatchedStripe(t *testing.T) {
	cfg := DiskConfig{FS: FSZFSRAID0, Disks: []Disk{disk("a", 100), disk("b", 40)}}
	warnings := cfg.Warnings()
	if len(warnings) == 0 {
		t.Fatal("a stripe over mismatched disks should warn")
	}
}

func TestUsableBytes(t *testing.T) {
	tests := []struct {
		name    string
		fs      FSType
		n       int
		wantGiB int64
	}{
		{"stripe adds every disk", FSZFSRAID0, 3, 120},
		{"mirror gives one disk", FSZFSRAID1, 2, 40},
		{"raid10 gives half", FSZFSRAID10, 4, 80},
		{"raidz1 loses one to parity", FSZFSRAIDZ1, 3, 80},
		{"raidz2 loses two", FSZFSRAIDZ2, 4, 80},
		{"raidz3 loses three", FSZFSRAIDZ3, 5, 80},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DiskConfig{FS: tt.fs, Disks: disks(tt.n, 40)}
			if got := cfg.UsableBytes() >> 30; got != tt.wantGiB {
				t.Errorf("UsableBytes() = %dG, want %dG", got, tt.wantGiB)
			}
		})
	}
}

// The parity arithmetic goes to zero and then negative as members run out, and
// a preview reading "-40GiB usable" is worse than no figure at all.
func TestUsableBytesIsZeroWhenTopologyCannotBeBuilt(t *testing.T) {
	tests := []struct {
		name string
		fs   FSType
		n    int
	}{
		{"raidz3 with exactly its parity count", FSZFSRAIDZ3, 3},
		{"raidz3 with fewer disks than parity", FSZFSRAIDZ3, 2},
		{"raidz2 with two disks", FSZFSRAIDZ2, 2},
		{"raidz1 with one disk", FSZFSRAIDZ1, 1},
		{"mirror with one disk", FSZFSRAID1, 1},
		{"raid10 with an odd count", FSZFSRAID10, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DiskConfig{FS: tt.fs, Disks: disks(tt.n, 40)}
			if cfg.Buildable() {
				t.Fatalf("Buildable() = true for %s across %d disks", tt.fs, tt.n)
			}
			if got := cfg.UsableBytes(); got != 0 {
				t.Errorf("UsableBytes() = %d, want 0", got)
			}
			if got := cfg.Tolerated(); got != 0 {
				t.Errorf("Tolerated() = %d, want 0", got)
			}
		})
	}
}

// A selection that cannot be built must not also be described as unredundant:
// the operator's problem is the disk count, not the topology.
func TestWarningsStaySilentUntilBuildable(t *testing.T) {
	cfg := DiskConfig{FS: FSZFSRAIDZ3, Disks: disks(3, 40)}
	if got := cfg.Warnings(); len(got) != 0 {
		t.Errorf("Warnings() = %v, want none", got)
	}
}

func TestRequirementNamesTheSameSizeRule(t *testing.T) {
	if got := (DiskConfig{FS: FSZFSRAIDZ2}).Requirement(); !strings.Contains(got, "4 disks") ||
		!strings.Contains(got, "same size") {
		t.Errorf("Requirement() = %q, want the disk count and the same-size rule", got)
	}
	if got := (DiskConfig{FS: FSZFSRAID10}).Requirement(); !strings.Contains(got, "even") {
		t.Errorf("Requirement() = %q, want the even-count rule", got)
	}
	// A stripe has no matching requirement, so claiming one would be wrong.
	if got := (DiskConfig{FS: FSZFSRAID0}).Requirement(); strings.Contains(got, "same size") {
		t.Errorf("Requirement() = %q, want no same-size rule for a stripe", got)
	}
}

func TestTolerated(t *testing.T) {
	tests := []struct {
		fs   FSType
		n    int
		want int
	}{
		{FSExt4, 1, 0},
		{FSZFSRAID0, 3, 0},
		{FSZFSRAID1, 2, 1},
		{FSZFSRAID1, 3, 2},
		{FSZFSRAID10, 4, 1},
		{FSZFSRAIDZ1, 3, 1},
		{FSZFSRAIDZ2, 4, 2},
		{FSZFSRAIDZ3, 5, 3},
	}
	for _, tt := range tests {
		t.Run(string(tt.fs), func(t *testing.T) {
			cfg := DiskConfig{FS: tt.fs, Disks: disks(tt.n, 40)}
			if got := cfg.Tolerated(); got != tt.want {
				t.Errorf("Tolerated() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseFSType(t *testing.T) {
	for _, fs := range AllFSTypes {
		got, err := ParseFSType(strings.ToUpper(string(fs)))
		if err != nil {
			t.Errorf("ParseFSType(%q): %v", fs, err)
			continue
		}
		if got != fs {
			t.Errorf("ParseFSType(%q) = %q", fs, got)
		}
	}
	// The error has to list the valid values: it is read off a serial console
	// by someone whose unattended install just stopped.
	_, err := ParseFSType("btrfs")
	if err == nil || !strings.Contains(err.Error(), "zfs-raidz1") {
		t.Fatalf("ParseFSType(btrfs) = %v, want an error listing the accepted values", err)
	}
}

func TestPartitionPathSeparator(t *testing.T) {
	tests := []struct{ dev, want string }{
		{"/dev/sda", "/dev/sda3"},
		// A trailing digit needs a 'p' or the name is ambiguous.
		{"/dev/nvme0n1", "/dev/nvme0n1p3"},
		{"/dev/vda", "/dev/vda3"},
	}
	for _, tt := range tests {
		if got := (Disk{Path: tt.dev}).PartitionPath(3); got != tt.want {
			t.Errorf("PartitionPath(%s) = %s, want %s", tt.dev, got, tt.want)
		}
	}
}

// udev names partition links with a -partN suffix regardless of the kernel's
// separator, so the by-id form must not reuse the 'p' rule.
func TestStablePartitionPathUsesPartSuffix(t *testing.T) {
	d := Disk{Path: "/dev/nvme0n1", ByID: "/dev/disk/by-id/nvme-SAMSUNG_1234"}
	if got := d.StablePartitionPath(3); got != "/dev/disk/by-id/nvme-SAMSUNG_1234-part3" {
		t.Errorf("StablePartitionPath() = %s", got)
	}
}

func TestESPSizeMiB(t *testing.T) {
	if got := espSizeMiB(disk("a", 40)); got != espSmallMiB {
		t.Errorf("espSizeMiB(40G) = %d, want %d", got, espSmallMiB)
	}
	if got := espSizeMiB(disk("a", 500)); got != espLargeMiB {
		t.Errorf("espSizeMiB(500G) = %d, want %d", got, espLargeMiB)
	}
}
