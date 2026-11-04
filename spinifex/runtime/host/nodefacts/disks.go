package nodefacts

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
)

// skipDiskPrefixes are block devices that are not drives: virtual, mapped,
// network or optical. The installer skips the same set.
var skipDiskPrefixes = []string{"ram", "loop", "md", "dm-", "fd", "sr", "zram", "nbd"}

// pseudoFSTypes are filesystems with no drive under them, or whose drive is an
// image file rather than storage the operator manages.
var pseudoFSTypes = map[string]bool{
	"autofs": true, "binfmt_misc": true, "bpf": true, "cgroup": true, "cgroup2": true,
	"configfs": true, "debugfs": true, "devpts": true, "devtmpfs": true, "efivarfs": true,
	"fuse.gvfsd-fuse": true, "fusectl": true, "hugetlbfs": true, "mqueue": true, "nsfs": true,
	"overlay": true, "proc": true, "pstore": true, "ramfs": true, "rpc_pipefs": true,
	"securityfs": true, "squashfs": true, "sysfs": true, "tmpfs": true, "tracefs": true,
}

// diskFacts lists the drives under /sys/block, as lsblk -d does.
func diskFacts(sysRoot string) []clusterv1.DiskFacts {
	entries, err := os.ReadDir(filepath.Join(sysRoot, "block"))
	if err != nil {
		return nil
	}
	var out []clusterv1.DiskFacts
	for _, e := range entries {
		name := e.Name()
		if slices.ContainsFunc(skipDiskPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			continue
		}
		dir := filepath.Join(sysRoot, "block", name)
		sectors, _ := strconv.ParseUint(readTrimmed(dir, "size"), 10, 64)
		// A zero-sized device is an empty card reader slot, not a drive.
		if sectors == 0 {
			continue
		}
		out = append(out, clusterv1.DiskFacts{
			Name:       name,
			Path:       "/dev/" + name,
			SizeBytes:  sectors * 512,
			Model:      readTrimmed(dir, "device", "model"),
			Transport:  transport(dir),
			Rotational: readTrimmed(dir, "queue", "rotational") == "1",
			Removable:  readTrimmed(dir, "removable") == "1",
		})
	}
	return out
}

// transport reads the bus from the device's sysfs path, as lsblk's TRAN does.
func transport(blockDir string) string {
	target, err := os.Readlink(blockDir)
	if err != nil {
		return ""
	}
	for seg := range strings.SplitSeq(filepath.ToSlash(target), "/") {
		switch {
		case seg == "nvme":
			return "nvme"
		case strings.HasPrefix(seg, "ata") && isDigits(seg[3:]):
			return "sata"
		case strings.HasPrefix(seg, "usb") && isDigits(seg[3:]):
			return "usb"
		case strings.HasPrefix(seg, "virtio") && isDigits(seg[6:]):
			return "virtio"
		}
	}
	return ""
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func readTrimmed(parts ...string) string {
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// notVisible is the Error of a mount another mount covers in the daemon's
// namespace, as ProtectHome covers /home, so its usage cannot be read here.
const notVisible = "not visible to the daemon"

// mountEntry is one line of mountinfo.
type mountEntry struct {
	id     string
	parent string
	device string // major:minor
	root   string
	mount  string
	fsType string
	source string
	hidden bool
}

// Filesystems lists mounted filesystems from mountinfo, without usage. A bind
// is named as one. The daemon's own sandbox is left out: directories systemd
// binds over themselves, and its PrivateTmp directories.
func Filesystems(env Env) []clusterv1.FilesystemFacts {
	sysRoot := env.SysRoot
	entries := mountEntries(filepath.Join(env.ProcRoot, "self", "mountinfo"))
	// The directory each device is mounted at whole, which a bind's root is
	// relative to.
	whole := map[string]string{}
	for _, m := range entries {
		if _, seen := whole[m.device]; !seen && m.root == "/" {
			whole[m.device] = m.mount
		}
	}
	byID := map[string]mountEntry{}
	for _, m := range entries {
		byID[m.id] = m
	}
	seen := map[string]bool{}
	var out []clusterv1.FilesystemFacts
	for _, m := range entries {
		key := m.device + " " + m.root + " " + m.mount
		if seen[key] || showsItsParent(byID[m.parent], m) || strings.Contains(m.root, "/systemd-private-") {
			continue
		}
		seen[key] = true
		fs := clusterv1.FilesystemFacts{Mount: m.mount, Source: m.source, Type: m.fsType, ID: m.id + " " + key}
		if m.hidden {
			fs.ID, fs.Error = "", notVisible
		}
		if base, ok := whole[m.device]; ok && m.root != "/" {
			fs.BindOf = filepath.Join(base, m.root)
		}
		fs.Disk = strings.Join(drivesUnder(sysRoot, m.device), ", ")
		out = append(out, fs)
	}
	return out
}

// mountEntries reads the real filesystems from mountinfo, marking as hidden
// those a different filesystem covers and every mount beneath them, except
// the covering mount and what is mounted on it.
// showsItsParent reports whether m mounts the directory its parent already
// shows at that path, as ProtectSystem and ReadWritePaths do.
func showsItsParent(parent, m mountEntry) bool {
	rel, err := filepath.Rel(parent.mount, m.mount)
	if parent.id == "" || err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	return parent.device == m.device && filepath.Join(parent.root, rel) == m.root
}

func mountEntries(path string) []mountEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []mountEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := slices.Index(fields, "-")
		if sep < 6 || len(fields) < sep+3 {
			continue
		}
		all = append(all, mountEntry{
			id:     fields[0],
			parent: fields[1],
			device: fields[2],
			root:   unescapeMount(fields[3]),
			mount:  unescapeMount(fields[4]),
			fsType: fields[sep+1],
			source: unescapeMount(fields[sep+2]),
		})
	}
	coveredBy := map[string]string{}
	parentOf := map[string]string{}
	byID := map[string]mountEntry{}
	for _, m := range all {
		parentOf[m.id], byID[m.id] = m.parent, m
	}
	for _, m := range all {
		// A re-mount of the same directory over itself still shows it.
		if p, ok := byID[m.parent]; ok && p.mount == m.mount && (p.device != m.device || p.root != m.root) {
			coveredBy[p.id] = m.id
		}
	}
	var out []mountEntry
	for _, m := range all {
		if pseudoFSTypes[m.fsType] || (m.fsType != "zfs" && !strings.HasPrefix(m.source, "/dev/")) {
			continue
		}
		below := ""
		for id, depth := m.id, 0; id != "" && depth < len(all); id, depth = parentOf[id], depth+1 {
			if c, ok := coveredBy[id]; ok && c != below {
				m.hidden = true
				break
			}
			below = id
		}
		out = append(out, m)
	}
	return out
}

// unescapeMount undoes the kernel's octal escaping of space, tab, newline and
// backslash in mountinfo paths.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// drivesUnder names the drives holding a device: a partition's parent, or for
// device-mapper and md, the drives under each of its slaves.
func drivesUnder(sysRoot, device string) []string {
	dir, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "dev", "block", device))
	if err != nil {
		return nil
	}
	return drivesOf(dir, 0)
}

func drivesOf(dir string, depth int) []string {
	if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
		return []string{filepath.Base(filepath.Dir(dir))}
	}
	slaves, err := os.ReadDir(filepath.Join(dir, "slaves"))
	if err != nil || len(slaves) == 0 || depth > 4 {
		return []string{filepath.Base(dir)}
	}
	var out []string
	for _, s := range slaves {
		next, err := filepath.EvalSymlinks(filepath.Join(dir, "slaves", s.Name()))
		if err != nil {
			continue
		}
		for _, d := range drivesOf(next, depth+1) {
			if !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
	}
	slices.Sort(out)
	return out
}
