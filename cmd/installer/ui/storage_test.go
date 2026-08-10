package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/cmd/installer/install"
)

// diskModel is a storage screen with n unselected disks and ext4 chosen, which
// is where the wizard starts.
func diskModel(n int) model {
	m := model{fsCursor: slices.Index(install.AllFSTypes, install.FSExt4)}
	for i := range n {
		m.disks = append(m.disks, install.Disk{
			Path:              string(rune('a'+i)) + "-disk",
			Bytes:             200 << 30,
			LogicalBlockSize:  512,
			PhysicalBlockSize: 512,
		})
	}
	return m
}

func rolePaths(cfg install.DiskConfig) map[install.DiskRole]string {
	out := map[install.DiskRole]string{}
	for _, rm := range cfg.Roles {
		out[rm.Role] = rm.Disk.Path
	}
	return out
}

// A single-disk machine should need no storage input at all, so the disk that
// newModel preselects has to arrive already holding the os role.
func TestPreselectedDiskIsAValidConfiguration(t *testing.T) {
	m := newModel([]install.Disk{{Path: "/dev/sda", Bytes: 200 << 30, LogicalBlockSize: 512}}, nil)
	cfg := m.storage()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the preselected disk must be installable as-is: %v", err)
	}
	if cfg.Primary().Path != "/dev/sda" {
		t.Errorf("Primary() = %q, want the preselected disk", cfg.Primary().Path)
	}
}

// Picking disks in order gives the layout an operator almost always wants, so
// the role key is only needed for the exceptions.
func TestSelectingDisksAssignsRolesInOrder(t *testing.T) {
	m := diskModel(3)
	for i := range 3 {
		m = m.toggleDisk(i)
	}
	got := rolePaths(m.storage())
	for role, want := range map[install.DiskRole]string{
		install.RoleOS: "a-disk", install.RoleSpinifex: "b-disk", install.RolePredastore: "c-disk",
	} {
		if got[role] != want {
			t.Errorf("%s = %q, want %q", role, got[role], want)
		}
	}
	if err := m.storage().Validate(); err != nil {
		t.Errorf("the default assignment must be valid: %v", err)
	}
}

// Deselecting has to drop the role with the disk, or every later role shifts
// onto the wrong drive.
func TestDeselectingADiskReleasesItsRole(t *testing.T) {
	m := diskModel(3)
	for i := range 3 {
		m = m.toggleDisk(i)
	}
	m = m.toggleDisk(1) // drop the spinifex drive

	got := rolePaths(m.storage())
	if got[install.RoleOS] != "a-disk" || got[install.RolePredastore] != "c-disk" {
		t.Fatalf("remaining roles moved: %v", got)
	}
	if _, ok := got[install.RoleSpinifex]; ok {
		t.Errorf("the spinifex role should be free again: %v", got)
	}
}

// The two-drive predastore layout is only reachable by reassigning, so cycling
// swaps rather than refusing a role another disk already holds.
func TestCycleRoleSwapsWithTheDiskThatHoldsIt(t *testing.T) {
	m := diskModel(2)
	m = m.toggleDisk(0).toggleDisk(1)
	// b-disk: spinifex → predastore, which no disk holds, so no swap.
	m = m.cycleRole(1)

	got := rolePaths(m.storage())
	if got[install.RoleOS] != "a-disk" || got[install.RolePredastore] != "b-disk" {
		t.Fatalf("roles = %v, want os on a-disk and predastore on b-disk", got)
	}

	// Cycling past the end wraps onto os, which a-disk holds, so they trade.
	m = m.cycleRole(1)
	got = rolePaths(m.storage())
	if got[install.RoleOS] != "b-disk" || got[install.RolePredastore] != "a-disk" {
		t.Fatalf("roles = %v, want the two disks to have swapped", got)
	}
	if err := m.storage().Validate(); err != nil {
		t.Errorf("a swap must always leave a valid assignment: %v", err)
	}
}

// ZFS pairs mirrors by selection order and has no roles to assign.
func TestStorageLeavesRolesUnsetOnZFS(t *testing.T) {
	m := diskModel(2)
	m.fsCursor = slices.Index(install.AllFSTypes, install.FSZFSRAID1)
	m = m.toggleDisk(0).toggleDisk(1)

	cfg := m.storage()
	if len(cfg.Roles) != 0 {
		t.Errorf("Roles = %v, want none on a pool", cfg.Roles)
	}
	if !slices.Equal(cfg.Paths(), []string{"a-disk", "b-disk"}) {
		t.Errorf("Paths() = %v, want the selection order preserved", cfg.Paths())
	}
}

// A three-drive selection looks like an array and is not one, so the mapping
// and the absence of redundancy both have to be on screen before the erase.
func TestConfirmScreenStatesTheLayoutAndTheRisk(t *testing.T) {
	m := diskModel(3)
	m.height = 40
	for i := range 3 {
		m = m.toggleDisk(i)
	}
	got := m.viewDiskConfirm(100)

	for _, want := range []string{"/var/lib/spinifex/predastore", "c-disk", "losing one loses"} {
		if !strings.Contains(got, want) {
			t.Errorf("confirm screen is missing %q:\n%s", want, got)
		}
	}
}
