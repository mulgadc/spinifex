package nodefacts

import (
	"os"
	"path/filepath"
	"testing"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(root, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

func linkTree(t *testing.T, root string, links map[string]string) {
	t.Helper()
	for link, target := range links {
		full := filepath.Join(root, link)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.Symlink(target, full))
	}
}

const (
	nvmeDev = "devices/pci0000:00/0000:00:01.1/0000:01:00.0/nvme/nvme0/nvme0n1"
	sataDev = "devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/block/sda"
	usbDev  = "devices/pci0000:00/0000:00:14.0/usb2/2-1/2-1:1.0/host6/target6:0:0/6:0:0:0/block/sdb"
	raidDev = "devices/pci0000:00/0000:00:02.0/0000:03:00.0/host1/target1:2:0/1:2:0:0/block/sdc"
	dmDev   = "devices/virtual/block/dm-0"
)

// A host with an NVMe system drive, a SATA data drive under LVM, a USB stick,
// a RAID controller's virtual disk and a loop device, laid out as sysfs is.
func diskHost(t *testing.T) Env {
	t.Helper()
	root := t.TempDir()
	sys := filepath.Join(root, "sys")
	writeTree(t, sys, map[string]string{
		nvmeDev + "/size":                  "3907029168",
		nvmeDev + "/device/model":          "Samsung SSD 990 PRO 2TB                 ",
		nvmeDev + "/queue/rotational":      "0",
		nvmeDev + "/removable":             "0",
		nvmeDev + "/nvme0n1p1/partition":   "1",
		nvmeDev + "/nvme0n1p2/partition":   "2",
		nvmeDev + "/nvme0n1p3/partition":   "3",
		sataDev + "/size":                  "15628053168",
		sataDev + "/device/model":          "ST8000NM000A-2KE",
		sataDev + "/queue/rotational":      "1",
		sataDev + "/removable":             "0",
		sataDev + "/sda1/partition":        "1",
		usbDev + "/size":                   "60437492",
		usbDev + "/device/model":           "Ultra Fit",
		usbDev + "/queue/rotational":       "0",
		usbDev + "/removable":              "1",
		raidDev + "/size":                  "1874329600",
		raidDev + "/device/model":          "PERC H730P Mini",
		raidDev + "/queue/rotational":      "0",
		raidDev + "/removable":             "0",
		dmDev + "/size":                    "15628050000",
		"devices/virtual/block/loop0/size": "131072",
	})
	linkTree(t, sys, map[string]string{
		"block/nvme0n1":   "../" + nvmeDev,
		"block/sda":       "../" + sataDev,
		"block/sdb":       "../" + usbDev,
		"block/sdc":       "../" + raidDev,
		"block/dm-0":      "../" + dmDev,
		"block/loop0":     "../devices/virtual/block/loop0",
		"dev/block/259:1": "../../" + nvmeDev + "/nvme0n1p1",
		"dev/block/259:2": "../../" + nvmeDev + "/nvme0n1p2",
		"dev/block/259:3": "../../" + nvmeDev + "/nvme0n1p3",
		"dev/block/253:0": "../../" + dmDev,
		"dev/block/8:1":   "../../" + sataDev + "/sda1",
	})
	// The volume group's one physical volume is sda's partition.
	linkTree(t, sys, map[string]string{dmDev + "/slaves/sda1": "../../../../../" + sataDev + "/sda1"})
	// The daemon's own view: ProtectSystem=full re-mounts /usr and /boot/efi
	// read-only over themselves, and ProtectHome=yes covers /home and the
	// mount beneath it.
	writeTree(t, root, map[string]string{"proc/self/mountinfo": `22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
23 22 259:1 / /boot/efi rw,relatime shared:2 - vfat /dev/nvme0n1p1 rw
24 22 253:0 / /mnt/disk2 rw,relatime shared:3 - xfs /dev/mapper/vg-data rw
25 22 253:0 /spinifex /var/lib/spinifex rw,relatime shared:3 - xfs /dev/mapper/vg-data rw
26 22 259:2 /usr /usr ro,relatime - ext4 /dev/nvme0n1p2 rw
27 23 259:1 / /boot/efi ro,relatime - vfat /dev/nvme0n1p1 rw
28 22 0:25 / /run rw,nosuid shared:5 - tmpfs tmpfs rw
29 22 0:26 / /sys/firmware/efi/efivars rw shared:6 - efivarfs efivarfs rw
30 22 0:40 / /tank rw shared:7 - zfs tank rw,xattr
31 22 8:1 / /mnt/old\040disk rw shared:8 - ext4 /dev/sda1 rw
32 22 7:0 / /snap/core/1 ro shared:9 - squashfs /dev/loop0 ro
33 22 259:3 / /home rw shared:10 - ext4 /dev/nvme0n1p3 rw
35 33 8:1 /backup /home/backup rw shared:8 - ext4 /dev/sda1 rw
34 33 0:25 /systemd/inaccessible/dir /home ro - tmpfs tmpfs rw
`})
	return Env{SysRoot: sys, ProcRoot: filepath.Join(root, "proc")}
}

func TestDisksListsDrivesWithTransportAndSkipsVirtualDevices(t *testing.T) {
	env := diskHost(t)

	assert.Equal(t, []clusterv1.DiskFacts{
		{Name: "nvme0n1", Path: "/dev/nvme0n1", SizeBytes: 3907029168 * 512, Model: "Samsung SSD 990 PRO 2TB", Transport: "nvme"},
		{Name: "sda", Path: "/dev/sda", SizeBytes: 15628053168 * 512, Model: "ST8000NM000A-2KE", Transport: "sata", Rotational: true},
		{Name: "sdb", Path: "/dev/sdb", SizeBytes: 60437492 * 512, Model: "Ultra Fit", Transport: "usb", Removable: true},
		{Name: "sdc", Path: "/dev/sdc", SizeBytes: 1874329600 * 512, Model: "PERC H730P Mini"},
	}, diskFacts(env.SysRoot))
}

func TestFilesystemsKeepsRealMountsNamesBindsAndDropsSelfRemounts(t *testing.T) {
	env := diskHost(t)

	assert.Equal(t, []clusterv1.FilesystemFacts{
		{Mount: "/", Source: "/dev/nvme0n1p2", Type: "ext4", Disk: "nvme0n1", ID: "22 259:2 / /"},
		{Mount: "/boot/efi", Source: "/dev/nvme0n1p1", Type: "vfat", Disk: "nvme0n1", ID: "23 259:1 / /boot/efi"},
		{Mount: "/mnt/disk2", Source: "/dev/mapper/vg-data", Type: "xfs", Disk: "sda", ID: "24 253:0 / /mnt/disk2"},
		{Mount: "/var/lib/spinifex", Source: "/dev/mapper/vg-data", Type: "xfs", Disk: "sda", BindOf: "/mnt/disk2/spinifex", ID: "25 253:0 /spinifex /var/lib/spinifex"},
		{Mount: "/tank", Source: "tank", Type: "zfs", ID: "30 0:40 / /tank"},
		{Mount: "/mnt/old disk", Source: "/dev/sda1", Type: "ext4", Disk: "sda", ID: "31 8:1 / /mnt/old disk"},
		{Mount: "/home", Source: "/dev/nvme0n1p3", Type: "ext4", Disk: "nvme0n1", Error: "not visible to the daemon"},
		{Mount: "/home/backup", Source: "/dev/sda1", Type: "ext4", Disk: "sda", BindOf: "/mnt/old disk/backup", Error: "not visible to the daemon"},
	}, Filesystems(env))
}

func TestBuildReportsDisksAndFilesystems(t *testing.T) {
	env := diskHost(t)
	env.Dir, env.EtcRoot = t.TempDir(), t.TempDir()

	f := Build(t.Context(), env)

	assert.Len(t, f.Disks, 4)
	assert.Len(t, f.Filesystems, 8)
}

func TestHiddenMountsAreNotProbed(t *testing.T) {
	var paths []string
	for _, m := range Mounts(Filesystems(diskHost(t))) {
		paths = append(paths, m.Path)
	}
	assert.Equal(t, []string{"/", "/boot/efi", "/mnt/disk2", "/var/lib/spinifex", "/tank", "/mnt/old disk"}, paths)
}
