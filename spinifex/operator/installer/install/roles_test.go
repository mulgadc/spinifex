package install

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// roleCfg builds an ext4 configuration from role/disk-name pairs, in whatever
// order they are given — WithRoles is responsible for normalising them.
func roleCfg(pairs ...any) DiskConfig {
	var roles []RoleMount
	for i := 0; i+1 < len(pairs); i += 2 {
		roles = append(roles, RoleMount{
			Role: pairs[i].(DiskRole),
			Disk: disk(pairs[i+1].(string), 100),
		})
	}
	return DiskConfig{FS: FSExt4}.WithRoles(roles)
}

// fakeUUIDs makes partUUID return a value derived from the device path, so a
// generated fstab can be checked without touching a real disk.
func fakeUUIDs(t *testing.T) {
	t.Helper()
	prev := partUUID
	partUUID = func(dev string) (string, error) { return "uuid" + strings.ReplaceAll(dev, "/", "-"), nil }
	t.Cleanup(func() { partUUID = prev })
}

func TestWithRolesNormalisesOrderAndDerivesDisks(t *testing.T) {
	// Given deepest-first, which is the order that breaks mounting.
	cfg := roleCfg(RolePredastore, "c", RoleSpinifex, "b", RoleOS, "a")

	wantRoles := []DiskRole{RoleOS, RoleSpinifex, RolePredastore}
	for i, rm := range cfg.Roles {
		if rm.Role != wantRoles[i] {
			t.Fatalf("role %d = %s, want %s", i, rm.Role, wantRoles[i])
		}
	}
	// Disks is derived, so the OS drive is first and every role drive appears
	// exactly once in the list the installer erases.
	if got := cfg.Paths(); !slices.Equal(got, []string{"/dev/a", "/dev/b", "/dev/c"}) {
		t.Fatalf("Paths() = %v, want the roles in canonical order", got)
	}
	if cfg.Primary().Path != "/dev/a" {
		t.Errorf("Primary() = %s, want the os drive", cfg.Primary().Path)
	}
}

func TestDataMountsExcludeTheOSDriveAndNestShallowestFirst(t *testing.T) {
	got := roleCfg(RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c").DataMounts()
	want := []string{"/var/lib/spinifex", "/var/lib/spinifex/predastore"}
	for i, rm := range got {
		if rm.Mountpoint() != want[i] {
			t.Fatalf("mount %d = %s, want %s", i, rm.Mountpoint(), want[i])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d data mounts, want %d", len(got), len(want))
	}
}

// A predastore drive without a spinifex drive is the two-drive layout, and it
// nests a mount inside the root filesystem rather than inside another mount.
func TestPredastoreRoleWithoutSpinifexIsValid(t *testing.T) {
	cfg := roleCfg(RoleOS, "a", RolePredastore, "b")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if got := cfg.DataMounts(); len(got) != 1 || got[0].Mountpoint() != "/var/lib/spinifex/predastore" {
		t.Fatalf("DataMounts() = %v, want predastore only", got)
	}
}

func TestValidateRoles(t *testing.T) {
	twoRoles := func(a, b DiskRole) DiskConfig { return roleCfg(a, "a", b, "b") }

	tests := []struct {
		name    string
		cfg     DiskConfig
		wantErr string
	}{
		{"single disk needs no roles", DiskConfig{FS: FSExt4, Disks: disks(1, 40)}, ""},
		{"os alone", roleCfg(RoleOS, "a"), ""},
		{"all three", roleCfg(RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c"), ""},
		{"no os role", twoRoles(RoleSpinifex, RolePredastore), "no disk assigned the os role"},
		{"duplicate role", twoRoles(RoleOS, RoleOS), "two disks are assigned the os role"},
		{"unknown role", roleCfg(RoleOS, "a", DiskRole("cache"), "b"), "no role assigned"},
		{"more disks than roles", roleCfg(
			RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c", DiskRole("cache"), "d"), "at most 3 disks"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// Roles describe where a workload lives on a plain filesystem. A pool already
// spans every member, so accepting them there would silently do nothing.
func TestValidateRejectsRolesOnZFS(t *testing.T) {
	cfg := DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)}
	cfg.Roles = []RoleMount{{Role: RoleOS, Disk: cfg.Disks[0]}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ext4 only") {
		t.Fatalf("Validate() = %v, want a roles-are-ext4-only error", err)
	}
}

func TestBootDisksAreOnlyTheOSDriveOnExt4(t *testing.T) {
	cfg := roleCfg(RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c")
	if got := cfg.bootDisks(); len(got) != 1 || got[0].Path != "/dev/a" {
		t.Fatalf("bootDisks() = %v, want the os drive only", got)
	}
	// Every ZFS member carries its own ESP, so all of them stay boot disks.
	zfs := DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)}
	if got := zfs.bootDisks(); len(got) != 2 {
		t.Fatalf("bootDisks() = %v, want every pool member", got)
	}
}

func TestWriteFstabEmitsRoleMountsShallowestFirst(t *testing.T) {
	root := withMountRoot(t)
	targetSkeleton(t, root)
	fakeUUIDs(t)

	cfg := roleCfg(RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c")
	if err := writeFstab(cfg); err != nil {
		t.Fatalf("writeFstab: %v", err)
	}
	got := readFile(t, root, "etc/fstab")

	// Order matters: `mount -a` in a rescue shell walks the file top-down, and
	// predastore cannot be mounted before the filesystem it sits inside.
	wantOrder := []string{" / ", "/boot/efi", " /var/lib/spinifex ", " /var/lib/spinifex/predastore "}
	at := -1
	for _, want := range wantOrder {
		i := strings.Index(got, want)
		if i < 0 {
			t.Fatalf("fstab is missing %q:\n%s", want, got)
		}
		if i < at {
			t.Fatalf("%q is out of order:\n%s", want, got)
		}
		at = i
	}

	for _, dev := range []string{"/dev/b1", "/dev/c1"} {
		if want := "UUID=uuid-dev-" + strings.TrimPrefix(dev, "/dev/"); !strings.Contains(got, want) {
			t.Errorf("fstab does not mount %s by UUID:\n%s", dev, got)
		}
	}
	// Data mounts are fsck'd after root, never alongside it.
	for line := range strings.SplitSeq(got, "\n") {
		if strings.Contains(line, "/var/lib/spinifex") && !strings.HasSuffix(line, "0 2") {
			t.Errorf("data mount should be fsck pass 2: %q", line)
		}
	}
}

// Fail-closed by design: a missing data drive must stop the boot for an admin,
// not let predastore write objects onto the OS drive while everything reports
// healthy. nofail would silently produce exactly that.
func TestWriteFstabNeverMarksRoleMountsNofail(t *testing.T) {
	root := withMountRoot(t)
	targetSkeleton(t, root)
	fakeUUIDs(t)

	if err := writeFstab(roleCfg(RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c")); err != nil {
		t.Fatalf("writeFstab: %v", err)
	}
	if got := readFile(t, root, "etc/fstab"); strings.Contains(got, "nofail") {
		t.Errorf("role mounts must not be nofail:\n%s", got)
	}
}

func TestWriteFstabSingleDiskIsUnchanged(t *testing.T) {
	root := withMountRoot(t)
	targetSkeleton(t, root)
	fakeUUIDs(t)

	if err := writeFstab(roleCfg(RoleOS, "a")); err != nil {
		t.Fatalf("writeFstab: %v", err)
	}
	if got := readFile(t, root, "etc/fstab"); strings.Contains(got, "/var/lib/spinifex") {
		t.Errorf("a single-disk install should mount nothing extra:\n%s", got)
	}
}

// rsync runs with --delete, so every mountpoint that exists before the copy
// needs protecting or the copy can delete across a mount boundary on a retry.
func TestMountProtectFiltersCoverEveryRoleMount(t *testing.T) {
	got := mountProtectFilters(roleCfg(RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c"))
	want := []string{"--filter=P /var/lib/spinifex", "--filter=P /var/lib/spinifex/predastore"}
	if !slices.Equal(got, want) {
		t.Fatalf("mountProtectFilters() = %v, want %v", got, want)
	}
	if got := mountProtectFilters(roleCfg(RoleOS, "a")); len(got) != 0 {
		t.Errorf("a single-disk install has no mounts to protect, got %v", got)
	}
	// ZFS keeps its dataset filters — the union is by mode, not additive.
	if got := mountProtectFilters(DiskConfig{FS: FSZFSRAID1, Disks: disks(2, 40)}); len(got) == 0 {
		t.Error("zfs mode should still protect its dataset mountpoints")
	}
}

func TestMountPartitionsMountsRolesAfterRootAndBeforeTheCopy(t *testing.T) {
	f := fakeCommands(t)
	root := withMountRoot(t)

	cfg := roleCfg(RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c")
	if err := mountPartitions(cfg); err != nil {
		t.Fatalf("mountPartitions: %v", err)
	}

	// The nested mount cannot precede the one it sits inside, or it is masked.
	want := []string{
		"mount /dev/a3 " + root,
		"mount /dev/a2 " + root + "/boot/efi",
		"mount /dev/b1 " + root + "/var/lib/spinifex",
		"mount /dev/c1 " + root + "/var/lib/spinifex/predastore",
	}
	var mounts []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "mount ") {
			mounts = append(mounts, c)
		}
	}
	if !slices.Equal(mounts, want) {
		t.Fatalf("mount order:\n got %v\nwant %v", mounts, want)
	}
}

func TestCleanupTargetUnmountsDeepestFirst(t *testing.T) {
	f := fakeCommands(t)
	root := withMountRoot(t)

	cleanupTarget(roleCfg(RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c"))

	// Unmounting the outer filesystem first fails with EBUSY and leaves a
	// half-mounted target behind for the next attempt.
	want := []string{
		"umount " + root + "/var/lib/spinifex/predastore",
		"umount " + root + "/var/lib/spinifex",
		"umount " + root + "/boot/efi",
		"umount " + root,
	}
	// The chroot binds are released first and are not part of the target order.
	var got []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "umount ") && slices.Contains(want, c) {
			got = append(got, c)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("unmount order:\n got %v\nwant %v", got, want)
	}
}

func TestPartitionDisksGivesDataDrivesOnePartitionAndNoESP(t *testing.T) {
	f := fakeCommands(t)
	nodes := tempDisks(t, 2, 200)
	cfg := DiskConfig{FS: FSExt4}.WithRoles(
		[]RoleMount{{Role: RoleOS, Disk: nodes[0]}, {Role: RoleSpinifex, Disk: nodes[1]}})
	osDisk, data := nodes[0].Path, nodes[1].Path

	if err := partitionDisks(cfg); err != nil {
		t.Fatalf("partitionDisks: %v", err)
	}

	f.mustRun(t, "sgdisk", osDisk, "-n 3:0:-1024M", "-t 3:8300")
	f.mustRun(t, "sgdisk", osDisk, "-t 2:EF00")
	// A data drive is never booted from, so a bios_boot or an ESP there would
	// only mislead whoever reads lsblk during an incident.
	for _, c := range f.calls {
		if strings.Contains(c, data) && (strings.Contains(c, "EF02") || strings.Contains(c, "EF00")) {
			t.Errorf("data drive must get no bios_boot or ESP: %q", c)
		}
	}
	f.mustRun(t, "sgdisk", data, "-n 1:1M:-1024M", "-t 1:8300", "-c 1:spinifex")
}

func TestFormatPartitionsLabelsEachRoleFilesystem(t *testing.T) {
	f := fakeCommands(t)
	cfg := roleCfg(RoleOS, "a", RoleSpinifex, "b", RolePredastore, "c")

	if err := formatPartitions(cfg); err != nil {
		t.Fatalf("formatPartitions: %v", err)
	}
	// One ESP only: a data drive holding a bootloader nothing points at would
	// mislead whoever reads lsblk during an incident.
	esps := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "mkfs.fat") {
			esps++
		}
	}
	if esps != 1 {
		t.Errorf("want one ESP, got %d:\n%v", esps, f.calls)
	}
	f.mustRun(t, "mkfs.ext4", "-L", "spinifex", "/dev/b1")
	f.mustRun(t, "mkfs.ext4", "-L", "predastore", "/dev/c1")
}

func TestWarningsFlagTheAbsenceOfRedundancy(t *testing.T) {
	got := strings.Join(roleCfg(RoleOS, "a", RoleSpinifex, "b").Warnings(), "\n")
	if !strings.Contains(got, "no redundancy") {
		t.Errorf("a multi-drive role layout must warn it is not an array, got %q", got)
	}
	if got := roleCfg(RoleOS, "a").Warnings(); len(got) != 0 {
		t.Errorf("a single-disk install has nothing to warn about, got %v", got)
	}
}

func TestWarningsFlagASmallPredastoreDrive(t *testing.T) {
	cfg := DiskConfig{FS: FSExt4}.WithRoles([]RoleMount{
		{Role: RoleOS, Disk: disk("a", 500)},
		{Role: RolePredastore, Disk: disk("b", 8)},
	})
	got := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(got, "small for object storage") {
		t.Errorf("an 8GiB predastore drive should be flagged, got %q", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the warning must not block the install: %v", err)
	}
}

func TestRolesForDisksAssignsPositionally(t *testing.T) {
	got := RolesForDisks(disks(3, 40))
	for i, rm := range got {
		if want := AllDiskRoles[i]; rm.Role != want {
			t.Errorf("disk %d got role %s, want %s", i, rm.Role, want)
		}
	}
	// A fourth disk has no role, which Validate is left to reject by name.
	if extra := RolesForDisks(disks(4, 40)); extra[3].Role != "" {
		t.Errorf("disk 4 got role %q, want none", extra[3].Role)
	}
}

func TestFourKnDataDriveIsAllowedWhereABootDriveIsNot(t *testing.T) {
	if bootedEFI() {
		t.Skip("machine booted in EFI mode; the 4Kn rule does not apply")
	}
	fourKn := func(name string) Disk {
		d := disk(name, 100)
		d.LogicalBlockSize = 4096
		return d
	}
	// GRUB's i386-pc target cannot read a 4Kn drive, but nothing boots from a
	// data drive, so only the OS role is constrained.
	ok := DiskConfig{FS: FSExt4}.WithRoles(
		[]RoleMount{{Role: RoleOS, Disk: disk("a", 100)}, {Role: RoleSpinifex, Disk: fourKn("b")}})
	if err := ok.Validate(); err != nil {
		t.Errorf("a 4Kn data drive should be accepted: %v", err)
	}

	bad := DiskConfig{FS: FSExt4}.WithRoles(
		[]RoleMount{{Role: RoleOS, Disk: fourKn("a")}, {Role: RoleSpinifex, Disk: disk("b", 100)}})
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "4Kn") {
		t.Errorf("Validate() = %v, want a 4Kn rejection for the os drive", err)
	}
}

func TestVerifyRoleLayoutCatchesAMountThatNeverHappened(t *testing.T) {
	root := withMountRoot(t)
	// Plain directories on one filesystem: exactly what a mount performed after
	// the rootfs copy, or not at all, leaves behind.
	if err := os.MkdirAll(filepath.Join(root, "var/lib/spinifex/predastore"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := verifyRoleLayout(roleCfg(RoleOS, "a", RoleSpinifex, "b"))
	if err == nil || !strings.Contains(err.Error(), "not a separate filesystem") {
		t.Fatalf("verifyRoleLayout() = %v, want a missing-mount error", err)
	}
	if !strings.Contains(fmt.Sprint(err), "/dev/b") {
		t.Errorf("the error must name the drive that was not mounted: %v", err)
	}
}
