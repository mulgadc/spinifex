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
// ordered and the order is significant: RAID10 pairs members two at a time.
type DiskConfig struct {
	FS    FSType
	Disks []Disk
	ZFS   ZFSOpts
}

// Primary is the first selected disk — the whole target in ext4 mode, and the
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
	if !d.FS.IsZFS() && len(d.Disks) > 1 {
		return fmt.Errorf("%s: supports a single disk only, %d selected — choose a zfs mode to use them all",
			d.FS.Label(), len(d.Disks))
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
		// GRUB's i386-pc target cannot read a 4Kn drive, so on legacy BIOS the
		// installed system would have no reachable bootloader.
		if !bootedEFI() && disk.LogicalBlockSize == 4096 {
			return fmt.Errorf("%s is a 4Kn drive (4096-byte sectors) and this machine booted in legacy BIOS mode — booting from it is not supported; enable UEFI in firmware",
				disk.Path)
		}
		if disk.Bytes < minRootBytes {
			return fmt.Errorf("%s is too small (%s, need at least %dGiB)",
				disk.Path, disk.SizeHuman(), minRootBytes>>30)
		}
	}

	return d.validateSizes()
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
	return out
}
