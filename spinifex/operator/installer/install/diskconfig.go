package install

import (
	"fmt"
	"math"
	"os"
	"strings"
)

// Pool and dataset names. rpool matches Proxmox, which is what every ZFS
// recovery guide and forum answer assumes — the name an operator will be
// reading off a serial console at 3am.
const (
	ZFSPoolName    = "rpool"
	ZFSRootDataset = ZFSPoolName + "/ROOT/spinifex"
)

// FSType selects the root filesystem and, for ZFS, the vdev topology.
type FSType string

const (
	FSExt4      FSType = "ext4"
	FSZFSRAID0  FSType = "zfs-raid0"
	FSZFSRAID1  FSType = "zfs-raid1"
	FSZFSRAID10 FSType = "zfs-raid10"
	FSZFSRAIDZ1 FSType = "zfs-raidz1"
	FSZFSRAIDZ2 FSType = "zfs-raidz2"
	FSZFSRAIDZ3 FSType = "zfs-raidz3"
)

// AllFSTypes is the selector order presented by the UI. ext4 is first and is
// the default, so ZFS — which holds the kernel at its shipped version and
// reserves RAM for the ARC — is always an explicit choice.
var AllFSTypes = []FSType{
	FSExt4, FSZFSRAID0, FSZFSRAID1, FSZFSRAID10, FSZFSRAIDZ1, FSZFSRAIDZ2, FSZFSRAIDZ3,
}

// fsMeta carries the per-topology facts that drive display and validation.
var fsMeta = map[FSType]struct {
	label     string
	minDisks  int
	redundant bool   // subject to the same-size rule
	vdev      string // zpool create keyword; empty means a plain stripe
}{
	FSExt4:      {"ext4", 1, false, ""},
	FSZFSRAID0:  {"zfs (RAID0)", 1, false, ""},
	FSZFSRAID1:  {"zfs (RAID1)", 2, true, "mirror"},
	FSZFSRAID10: {"zfs (RAID10)", 4, true, "mirror"},
	FSZFSRAIDZ1: {"zfs (RAIDZ-1)", 3, true, "raidz1"},
	FSZFSRAIDZ2: {"zfs (RAIDZ-2)", 4, true, "raidz2"},
	FSZFSRAIDZ3: {"zfs (RAIDZ-3)", 5, true, "raidz3"},
}

// IsZFS reports whether this mode creates a pool rather than a plain filesystem.
func (f FSType) IsZFS() bool { return strings.HasPrefix(string(f), "zfs-") }

// Label is the display name, matching proxinstall's wording so the screen is
// recognisable to anyone who has installed Proxmox.
func (f FSType) Label() string { return fsMeta[f].label }

// MinDisks is the smallest member count the topology can be built from.
func (f FSType) MinDisks() int { return fsMeta[f].minDisks }

// Redundant reports whether the topology stores parity or mirrors, and so
// requires members of matching size. A RAID0 stripe over unequal disks is
// legitimate and is deliberately not covered.
func (f FSType) Redundant() bool { return fsMeta[f].redundant }

// vdevKeyword is the zpool create keyword introducing each vdev.
func (f FSType) vdevKeyword() string { return fsMeta[f].vdev }

// ParseFSType maps a SPINIFEX_FS value to an FSType.
func ParseFSType(s string) (FSType, error) {
	f := FSType(strings.ToLower(strings.TrimSpace(s)))
	if _, ok := fsMeta[f]; !ok {
		names := make([]string, 0, len(AllFSTypes))
		for _, t := range AllFSTypes {
			names = append(names, string(t))
		}
		return "", fmt.Errorf("unknown filesystem %q — must be one of: %s", s, strings.Join(names, ", "))
	}
	return f, nil
}

// DiskRole is the purpose a selected drive serves on an ext4 install. A ZFS
// pool carries every workload on one set of members, so roles do not apply
// there — the topology already spans all the disks.
type DiskRole string

const (
	RoleOS         DiskRole = "os"
	RoleSpinifex   DiskRole = "spinifex"
	RolePredastore DiskRole = "predastore"
)

// AllDiskRoles is canonical order, outermost mountpoint first. Mounting, fstab
// emission and rsync protect filters all walk it forwards; teardown walks it
// back, because predastore nests inside spinifex.
var AllDiskRoles = []DiskRole{RoleOS, RoleSpinifex, RolePredastore}

// roleMountpoints is where each role lands on the installed system. A role left
// unassigned simply stays on the filesystem above it.
var roleMountpoints = map[DiskRole]string{
	RoleOS:         "/",
	RoleSpinifex:   "/var/lib/spinifex",
	RolePredastore: "/var/lib/spinifex/predastore",
}

// Label is the role name as shown in the UI and written to partition labels.
func (r DiskRole) Label() string { return string(r) }

// Mountpoint is the role's path on the installed system.
func (r DiskRole) Mountpoint() string { return roleMountpoints[r] }

// RoleMount binds one drive to one role.
type RoleMount struct {
	Role DiskRole
	Disk Disk
}

// Mountpoint is where this drive's filesystem is mounted on the target.
func (r RoleMount) Mountpoint() string { return r.Role.Mountpoint() }

// ZFSOpts are the advanced tunables. A zero value means "use the computed
// default", which is why Ashift and ARCMaxMiB are plain ints — 0 is not legal
// for either, so it is unambiguous as a sentinel.
type ZFSOpts struct {
	Ashift    int
	Compress  string
	Checksum  string
	Copies    int
	ARCMaxMiB int
}

// DiskConfig is the storage half of the installer's configuration. Disks is
// ordered and the order is significant: RAID10 pairs members two at a time, and
// on ext4 the first entry is the OS drive.
type DiskConfig struct {
	FS    FSType
	Disks []Disk
	ZFS   ZFSOpts

	// Roles assigns ext4 drives to mountpoints, in AllDiskRoles order. Empty
	// means a single-disk install with everything on the root filesystem.
	Roles []RoleMount
}

// WithRoles returns a copy with ext4 drive roles assigned and Disks rebuilt
// from them in canonical order, so the list of drives to erase and the list of
// roles cannot drift apart — there is one place a drive can be named, not two.
func (d DiskConfig) WithRoles(roles []RoleMount) DiskConfig {
	d.Roles, d.Disks = nil, nil
	add := func(rm RoleMount) {
		d.Roles = append(d.Roles, rm)
		d.Disks = append(d.Disks, rm.Disk)
	}
	for _, want := range AllDiskRoles {
		for _, rm := range roles {
			if rm.Role == want {
				add(rm)
			}
		}
	}
	// Anything with an unknown or duplicate role is kept so Validate can name
	// it, rather than being dropped into a silently smaller selection.
	for _, rm := range roles {
		if _, known := roleMountpoints[rm.Role]; !known {
			add(rm)
		}
	}
	return d
}

// RolesForDisks assigns roles positionally, which is the default the UI offers:
// first disk picked is the OS, second is spinifex, third is predastore.
func RolesForDisks(disks []Disk) []RoleMount {
	out := make([]RoleMount, 0, len(disks))
	for i, disk := range disks {
		var role DiskRole
		if i < len(AllDiskRoles) {
			role = AllDiskRoles[i]
		}
		out = append(out, RoleMount{Role: role, Disk: disk})
	}
	return out
}

// DataMounts is every role except the OS drive, shallowest mountpoint first.
// This is the traversal every stage of the install shares.
func (d DiskConfig) DataMounts() []RoleMount {
	var out []RoleMount
	for _, rm := range d.Roles {
		if rm.Role != RoleOS {
			out = append(out, rm)
		}
	}
	return out
}

// bootDisks are the drives a bootloader is installed to. Every ZFS member gets
// its own ESP; on ext4 only the OS drive does, because a data drive holding a
// bootloader that nothing points at is a trap during recovery.
func (d DiskConfig) bootDisks() []Disk {
	if d.FS.IsZFS() || len(d.Disks) == 0 {
		return d.Disks
	}
	return d.Disks[:1]
}

// Primary is the first selected disk — the OS drive in ext4 mode, and the
// disk shown in single-disk summaries.
func (d DiskConfig) Primary() Disk {
	if len(d.Disks) == 0 {
		return Disk{}
	}
	return d.Disks[0]
}

// Paths lists the selected device paths, for logs and confirmation screens.
func (d DiskConfig) Paths() []string {
	out := make([]string, len(d.Disks))
	for i, disk := range d.Disks {
		out[i] = disk.Path
	}
	return out
}

// Buildable reports whether the selection has enough members, of the right
// count, to form the topology at all. Capacity and fault tolerance are
// meaningless until it does — a 3-disk RAIDZ-3 has no geometry to describe.
func (d DiskConfig) Buildable() bool {
	if _, ok := fsMeta[d.FS]; !ok {
		return false
	}
	if len(d.Disks) < d.FS.MinDisks() {
		return false
	}
	return d.FS != FSZFSRAID10 || len(d.Disks)%2 == 0
}

// Requirement describes what the topology still needs, for the preview line
// shown in place of a capacity that cannot be computed yet.
func (d DiskConfig) Requirement() string {
	req := fmt.Sprintf("needs %d disks", d.FS.MinDisks())
	if d.FS == FSZFSRAID10 {
		req = "needs an even number of disks, at least 4"
	}
	if d.FS.Redundant() {
		req += " of the same size"
	}
	return req
}

// UsableBytes estimates post-parity capacity, for the geometry preview. It is
// an operator-facing approximation and deliberately ignores metadata overhead.
// Returns 0 for a selection that cannot form the topology, since the parity
// arithmetic below goes to zero and then negative as members run out.
func (d DiskConfig) UsableBytes() int64 {
	n := int64(len(d.Disks))
	if n == 0 || !d.Buildable() {
		return 0
	}
	smallest := d.Disks[0].Bytes
	for _, disk := range d.Disks {
		smallest = min(smallest, disk.Bytes)
	}
	switch d.FS {
	case FSExt4, FSZFSRAID0:
		var total int64
		for _, disk := range d.Disks {
			total += disk.Bytes
		}
		return total
	case FSZFSRAID1:
		return smallest
	case FSZFSRAID10:
		return smallest * (n / 2)
	case FSZFSRAIDZ1:
		return smallest * (n - 1)
	case FSZFSRAIDZ2:
		return smallest * (n - 2)
	case FSZFSRAIDZ3:
		return smallest * (n - 3)
	}
	return 0
}

// Tolerated returns the number of simultaneous disk failures the topology
// survives, for the geometry preview.
func (d DiskConfig) Tolerated() int {
	if !d.Buildable() {
		return 0
	}
	switch d.FS {
	case FSZFSRAID1:
		return max(len(d.Disks)-1, 0)
	case FSZFSRAID10:
		return 1 // one per mirror pair; the honest worst case is one
	case FSZFSRAIDZ1:
		return 1
	case FSZFSRAIDZ2:
		return 2
	case FSZFSRAIDZ3:
		return 3
	}
	return 0
}

// sizeTolerance is the fraction by which pool members may differ in size.
// Matches Proxmox's zfs_mirror_size_check: anything beyond 10% and the pool
// silently truncates every member to the smallest.
const sizeTolerance = 0.10

// bootedEFI reports whether firmware booted this machine in EFI mode. GRUB's
// i386-pc target cannot read a 4Kn drive, so legacy BIOS constrains selection.
func bootedEFI() bool {
	_, err := os.Stat("/sys/firmware/efi")
	return err == nil
}

// Validate checks a disk selection against every rule that must hold before any
// destructive command runs. The UI calls it on each keystroke so failures block
// Continue, rather than aborting an install that has already repartitioned.
func (d DiskConfig) Validate() error {
	if _, ok := fsMeta[d.FS]; !ok {
		return fmt.Errorf("no filesystem selected")
	}
	if len(d.Disks) == 0 {
		return fmt.Errorf("%s: select at least one disk", d.FS.Label())
	}
	if err := d.validateRoles(); err != nil {
		return err
	}
	if n := len(d.Disks); n < d.FS.MinDisks() {
		return fmt.Errorf("%s: needs at least %d disks, %d selected", d.FS.Label(), d.FS.MinDisks(), n)
	}
	if d.FS == FSZFSRAID10 && len(d.Disks)%2 != 0 {
		return fmt.Errorf("%s: needs an even number of disks, %d selected", d.FS.Label(), len(d.Disks))
	}

	seen := map[string]bool{}
	for _, disk := range d.Disks {
		if seen[disk.Path] {
			return fmt.Errorf("%s is selected more than once", disk.Path)
		}
		seen[disk.Path] = true

		if disk.LiveMedia {
			return fmt.Errorf("%s is the installer's own boot media and cannot be erased", disk.Path)
		}
		if disk.Bytes < minRootBytes {
			return fmt.Errorf("%s is too small (%s, need at least %dGiB)",
				disk.Path, disk.SizeHuman(), minRootBytes>>30)
		}
	}

	// GRUB's i386-pc target cannot read a 4Kn drive, so on legacy BIOS a drive
	// holding a bootloader would leave the installed system unreachable. Only
	// boot drives are constrained: a data role is never booted from.
	for _, disk := range d.bootDisks() {
		if !bootedEFI() && disk.LogicalBlockSize == 4096 {
			return fmt.Errorf("%s is a 4Kn drive (4096-byte sectors) and this machine booted in legacy BIOS mode — booting from it is not supported; enable UEFI in firmware",
				disk.Path)
		}
	}

	return d.validateSizes()
}

// validateRoles checks the ext4 role assignment. Roles are the only way to use
// more than one disk without ZFS, so a multi-disk selection without them is
// rejected rather than quietly installed onto the first drive.
func (d DiskConfig) validateRoles() error {
	if d.FS.IsZFS() {
		if len(d.Roles) > 0 {
			return fmt.Errorf("%s: drive roles apply to ext4 only — a pool spans every member already", d.FS.Label())
		}
		return nil
	}
	if len(d.Roles) == 0 {
		if len(d.Disks) > 1 {
			return fmt.Errorf("%s: %d disks selected but no roles assigned — assign os, spinifex and predastore, or choose a zfs mode",
				d.FS.Label(), len(d.Disks))
		}
		return nil
	}
	if len(d.Roles) > len(AllDiskRoles) {
		return fmt.Errorf("%s: at most %d disks, %d selected — the roles are os, spinifex and predastore",
			d.FS.Label(), len(AllDiskRoles), len(d.Roles))
	}

	seen := map[DiskRole]bool{}
	for _, rm := range d.Roles {
		if _, ok := roleMountpoints[rm.Role]; !ok {
			return fmt.Errorf("%s has no role assigned — every selected disk needs one", rm.Disk.Path)
		}
		if seen[rm.Role] {
			return fmt.Errorf("two disks are assigned the %s role", rm.Role)
		}
		seen[rm.Role] = true
	}
	if !seen[RoleOS] {
		return fmt.Errorf("%s: no disk assigned the os role — one disk must hold the operating system", d.FS.Label())
	}
	if len(d.Roles) != len(d.Disks) {
		return fmt.Errorf("internal: %d roles for %d disks", len(d.Roles), len(d.Disks))
	}
	return nil
}

// minRootBytes matches Proxmox's hard floor for a root disk.
const minRootBytes = 2 << 30

// validateSizes enforces the same-size rule where the topology needs it:
// across all members for mirrors and raidz, and per-pair for RAID10 — a
// striped mirror only requires each pair to match, not every disk in the pool.
func (d DiskConfig) validateSizes() error {
	if !d.FS.Redundant() {
		return nil
	}
	if d.FS == FSZFSRAID10 {
		for i := 0; i+1 < len(d.Disks); i += 2 {
			if err := sizesMatch(d.Disks[i], d.Disks[i+1]); err != nil {
				return fmt.Errorf("%s mirror pair %d: %w", d.FS.Label(), i/2+1, err)
			}
		}
		return nil
	}
	for _, disk := range d.Disks[1:] {
		if err := sizesMatch(d.Disks[0], disk); err != nil {
			return fmt.Errorf("%s: %w", d.FS.Label(), err)
		}
	}
	return nil
}

// SizesWithinTolerance reports whether two disks could be members of the same
// redundant pool. The UI uses it to decide whether suggesting ZFS is honest.
func SizesWithinTolerance(a, b Disk) bool { return sizesMatch(a, b) == nil }

// sizesMatch reports whether two members are within the tolerance, naming both
// disks and their sizes — that message is the operator's only diagnostic.
func sizesMatch(a, b Disk) error {
	if math.Abs(float64(a.Bytes-b.Bytes)) > float64(a.Bytes)*sizeTolerance {
		return fmt.Errorf("disks must be the same size within %.0f%%: %s is %s, %s is %s",
			sizeTolerance*100, a.Path, a.SizeHuman(), b.Path, b.SizeHuman())
	}
	return nil
}

// Warnings returns non-blocking advice for a valid selection. A RAID0 stripe
// over mismatched disks is legal but almost always a mistake, so it is
// surfaced without preventing the install — which is what Proxmox does.
func (d DiskConfig) Warnings() []string {
	var out []string
	if d.FS == FSZFSRAID0 && len(d.Disks) > 1 {
		for _, disk := range d.Disks[1:] {
			if sizesMatch(d.Disks[0], disk) != nil {
				out = append(out, "striping disks of different sizes — capacity is usable but a failure of any disk loses the pool")
				break
			}
		}
	}
	if d.FS.IsZFS() && d.FS != FSZFSRAID0 && d.Buildable() && d.Tolerated() == 0 {
		out = append(out, "this topology stores no redundancy")
	}
	// A three-drive role layout looks like an array and is not one: each drive
	// is an independent point of failure, so the node loses whatever sat on it.
	if len(d.DataMounts()) > 0 {
		out = append(out, "role-assigned drives store no redundancy — losing any one drive loses what it held")
	}
	for _, rm := range d.DataMounts() {
		if rm.Role == RolePredastore && rm.Disk.Bytes < smallPredastoreBytes {
			out = append(out, fmt.Sprintf("%s is small for object storage (%s) — predastore will fill quickly",
				rm.Disk.Path, rm.Disk.SizeHuman()))
		}
	}
	return out
}

// smallPredastoreBytes is where a dedicated object-store drive stops being
// worth the drive. Advisory only: the install is legal below it.
const smallPredastoreBytes = 64 << 30
