package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// The in-guest half of a storage grow (D12). The control plane has already
// grown the block device by the time this runs; all that is left is to put the
// filesystem on the capacity that is now there. Both ext4 and XFS grow while
// mounted, so this needs no ordering against the engine start.
type storageOps interface {
	// Extends the filesystem at the data mount onto its whole device, and
	// reports what it did for the command reply.
	GrowFilesystem(ctx context.Context) (string, error)
}

// Tool names rather than paths: the image installs them from
// cloud-utils-growpart, e2fsprogs-extra and xfsprogs, all on PATH.
const (
	defaultGrowpart   = "growpart"
	defaultResize2fs  = "resize2fs"
	defaultXFSGrowfs  = "xfs_growfs"
	defaultMountsFile = "/proc/mounts"
	defaultSysBlock   = "/sys/class/block"
	// Where rds-datadir mounts the data volume; the datadir sits one level in.
	defaultDataMount = "/var/lib/postgresql"
	// growpart says this when the partition already fills the disk, which a
	// resumed grow is the normal way to reach.
	growpartNoChange = "NOCHANGE"
)

type guestStorage struct {
	run        commandRunner
	dataMount  string
	mountsFile string
	sysBlock   string
	growpart   string
	resize2fs  string
	xfsGrowfs  string
}

var _ storageOps = (*guestStorage)(nil)

func newGuestStorage(cfg config, run commandRunner) *guestStorage {
	return &guestStorage{
		run:        run,
		dataMount:  cfg.DataMount,
		mountsFile: cfg.MountsFile,
		sysBlock:   cfg.SysBlock,
		growpart:   defaultGrowpart,
		resize2fs:  defaultResize2fs,
		xfsGrowfs:  defaultXFSGrowfs,
	}
}

// The device and filesystem behind the data mount, resolved from the kernel's
// own mount table rather than from anything the control plane asserted.
type dataMount struct {
	device string
	fstype string
}

func (g *guestStorage) GrowFilesystem(ctx context.Context) (string, error) {
	mount, err := g.resolveDataMount()
	if err != nil {
		return "", err
	}
	// rds-datadir formats the whole device and refuses a disk that already
	// carries a partition table, so the data volume is normally unpartitioned
	// and there is no partition to push out first.
	if err := g.growPartition(ctx, mount.device); err != nil {
		return "", err
	}

	switch {
	case strings.HasPrefix(mount.fstype, "ext"):
		// resize2fs grows a mounted ext filesystem online, and takes the device.
		if _, err := g.run(ctx, command{
			Name: g.resize2fs,
			Args: []string{mount.device},
			Env:  []string{"PATH=" + defaultGuestPath},
		}); err != nil {
			return "", fmt.Errorf("grow the %s filesystem on %s: %w", mount.fstype, mount.device, err)
		}
	case mount.fstype == "xfs":
		// xfs_growfs is addressed by mount point, and XFS can only grow mounted.
		if _, err := g.run(ctx, command{
			Name: g.xfsGrowfs,
			Args: []string{g.dataMount},
			Env:  []string{"PATH=" + defaultGuestPath},
		}); err != nil {
			return "", fmt.Errorf("grow the xfs filesystem at %s: %w", g.dataMount, err)
		}
	default:
		// Guessing a tool here would either do nothing or corrupt a filesystem
		// this image was never meant to be holding.
		return "", fmt.Errorf("%s holds an unsupported filesystem %q; refusing to grow it", mount.device, mount.fstype)
	}

	slog.Info("rds-agent: data filesystem grown", "device", mount.device, "fstype", mount.fstype, "mount", g.dataMount)
	return fmt.Sprintf("grew %s (%s) at %s", mount.device, mount.fstype, g.dataMount), nil
}

// Pushes the partition out to the end of its disk when the data mount happens
// to sit on one. A device that is not a partition needs nothing, and a
// partition that already fills its disk is the normal state of a resumed grow.
func (g *guestStorage) growPartition(ctx context.Context, device string) error {
	disk, number, ok := g.partitionOf(device)
	if !ok {
		return nil
	}
	out, err := g.run(ctx, command{
		Name: g.growpart,
		Args: []string{disk, number},
		Env:  []string{"PATH=" + defaultGuestPath},
	})
	if err == nil {
		return nil
	}
	if strings.Contains(out, growpartNoChange) || strings.Contains(err.Error(), growpartNoChange) {
		return nil
	}
	return fmt.Errorf("grow partition %s of %s: %w", number, disk, err)
}

// Whether device is a partition, and if so its disk and number. Read from
// sysfs, which states it directly rather than inferring it from a trailing
// digit — nvme0n1 is a disk and vdb1 is a partition on the same rule.
func (g *guestStorage) partitionOf(device string) (disk, number string, ok bool) {
	name := filepath.Base(device)
	raw, err := os.ReadFile(filepath.Join(g.sysBlock, name, "partition"))
	if err != nil {
		return "", "", false
	}
	number = strings.TrimSpace(string(raw))
	if number == "" {
		return "", "", false
	}
	// A partition's sysfs entry hangs off its disk's, so the parent directory
	// names the disk without any parsing of the device name.
	target, err := os.Readlink(filepath.Join(g.sysBlock, name))
	if err != nil {
		return "", "", false
	}
	return filepath.Join(filepath.Dir(device), filepath.Base(filepath.Dir(target))), number, true
}

// The mount table entry for the data mount. An absent one means the data volume
// is not mounted, which is a broken guest rather than something to grow.
func (g *guestStorage) resolveDataMount() (dataMount, error) {
	f, err := os.Open(g.mountsFile)
	if err != nil {
		return dataMount{}, fmt.Errorf("read %s: %w", g.mountsFile, err)
	}
	defer f.Close()

	// Later entries shadow earlier ones at the same point, so the last match is
	// the filesystem actually visible there.
	found := dataMount{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[1] != g.dataMount {
			continue
		}
		found = dataMount{device: fields[0], fstype: fields[2]}
	}
	if err := scanner.Err(); err != nil {
		return dataMount{}, fmt.Errorf("read %s: %w", g.mountsFile, err)
	}
	if found.device == "" {
		return dataMount{}, fmt.Errorf("no filesystem is mounted at %s; the data volume is not attached", g.dataMount)
	}
	return found, nil
}
