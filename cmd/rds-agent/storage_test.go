package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// Stands in for the guest's storage in the registry tests, where what matters
// is that the command reaches it rather than what it does to a filesystem.
type fakeStorage struct {
	grown   bool
	message string
	err     error
}

var _ storageOps = (*fakeStorage)(nil)

func (f *fakeStorage) GrowFilesystem(context.Context) (string, error) {
	f.grown = true
	return f.message, f.err
}

// runLog records what a grow shelled out to, in order, so the test asserts on
// the tools invoked rather than on a filesystem it would have to create.
type runLog struct {
	calls []command
	// errOn fails the first command whose name matches, which is how growpart's
	// tolerated failure and resize2fs's fatal one are told apart.
	errOn  string
	errOut string
	err    error
}

func (r *runLog) run(_ context.Context, c command) (string, error) {
	r.calls = append(r.calls, c)
	if r.errOn != "" && c.Name == r.errOn {
		return r.errOut, r.err
	}
	return "", nil
}

func (r *runLog) names() []string {
	names := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		names = append(names, c.Name)
	}
	return names
}

// A guest whose data volume is mounted as fstype from device, with a sysfs tree
// that reports the device as a whole disk unless the test says otherwise.
func newTestStorage(t *testing.T, device, fstype string, run commandRunner) *guestStorage {
	t.Helper()
	dir := t.TempDir()
	mounts := filepath.Join(dir, "mounts")
	content := "proc /proc proc rw 0 0\n"
	if device != "" {
		content += device + " " + defaultDataMount + " " + fstype + " rw,relatime 0 0\n"
	}
	if err := os.WriteFile(mounts, []byte(content), 0o600); err != nil {
		t.Fatalf("write the mount table: %v", err)
	}
	cfg := config{DataMount: defaultDataMount, MountsFile: mounts, SysBlock: filepath.Join(dir, "block")}
	if err := os.MkdirAll(cfg.SysBlock, 0o755); err != nil {
		t.Fatalf("create the sysfs tree: %v", err)
	}
	return newGuestStorage(cfg, run)
}

// Makes name look like partition number of disk, the way sysfs states it: a
// "partition" file under the device, hanging off its disk's directory.
func seedPartition(t *testing.T, g *guestStorage, disk, name, number string) {
	t.Helper()
	// sysfs holds the real entry under the device tree and links to it from
	// /sys/class/block, so the partition's disk is its link target's parent.
	target := filepath.Join("..", "devices", disk, name)
	if err := os.MkdirAll(filepath.Join(g.sysBlock, target), 0o755); err != nil {
		t.Fatalf("create the partition tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(g.sysBlock, target, "partition"), []byte(number+"\n"), 0o600); err != nil {
		t.Fatalf("write the partition number: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(g.sysBlock, name)); err != nil {
		t.Fatalf("link the partition: %v", err)
	}
}

// The data volume is formatted whole, so the usual grow is resize2fs against
// the device with no partition to push out first.
func TestGrowFilesystem_ResizesAnExt4DeviceOnline(t *testing.T) {
	log := &runLog{}
	g := newTestStorage(t, "/dev/vdb", "ext4", log.run)

	message, err := g.GrowFilesystem(context.Background())
	if err != nil {
		t.Fatalf("GrowFilesystem: %v", err)
	}

	if got := log.names(); len(got) != 1 || got[0] != defaultResize2fs {
		t.Fatalf("ran %v, want just %s", got, defaultResize2fs)
	}
	if args := log.calls[0].Args; len(args) != 1 || args[0] != "/dev/vdb" {
		t.Errorf("resize2fs args = %v, want the device", args)
	}
	if !strings.Contains(message, "/dev/vdb") || !strings.Contains(message, defaultDataMount) {
		t.Errorf("reply message = %q, want the device and mount it grew", message)
	}
}

// XFS is addressed by mount point and can only grow mounted, unlike ext.
func TestGrowFilesystem_GrowsXFSByMountPoint(t *testing.T) {
	log := &runLog{}
	g := newTestStorage(t, "/dev/vdb", "xfs", log.run)

	if _, err := g.GrowFilesystem(context.Background()); err != nil {
		t.Fatalf("GrowFilesystem: %v", err)
	}

	if got := log.names(); len(got) != 1 || got[0] != defaultXFSGrowfs {
		t.Fatalf("ran %v, want just %s", got, defaultXFSGrowfs)
	}
	if args := log.calls[0].Args; len(args) != 1 || args[0] != defaultDataMount {
		t.Errorf("xfs_growfs args = %v, want the mount point", args)
	}
}

// A partitioned data device has to have the partition pushed out to the end of
// the disk before the filesystem can be extended onto it.
func TestGrowFilesystem_PushesThePartitionOutFirst(t *testing.T) {
	log := &runLog{}
	g := newTestStorage(t, "/dev/vdb1", "ext4", log.run)
	seedPartition(t, g, "vdb", "vdb1", "1")

	if _, err := g.GrowFilesystem(context.Background()); err != nil {
		t.Fatalf("GrowFilesystem: %v", err)
	}

	if got := log.names(); len(got) != 2 || got[0] != defaultGrowpart || got[1] != defaultResize2fs {
		t.Fatalf("ran %v, want growpart then resize2fs", got)
	}
	if args := log.calls[0].Args; len(args) != 2 || args[0] != "/dev/vdb" || args[1] != "1" {
		t.Errorf("growpart args = %v, want the disk and the partition number", args)
	}
}

// A resumed grow finds the partition already filling its disk, which growpart
// reports as a failure. Treating it as one would fail a grow that has nothing
// left to do at that step.
func TestGrowFilesystem_ToleratesAnAlreadyFullPartition(t *testing.T) {
	log := &runLog{errOn: defaultGrowpart, errOut: "NOCHANGE: partition 1 is size 41940992. it cannot be grown", err: errors.New("exit status 1")}
	g := newTestStorage(t, "/dev/vdb1", "ext4", log.run)
	seedPartition(t, g, "vdb", "vdb1", "1")

	if _, err := g.GrowFilesystem(context.Background()); err != nil {
		t.Fatalf("GrowFilesystem: %v", err)
	}
	if got := log.names(); len(got) != 2 || got[1] != defaultResize2fs {
		t.Errorf("ran %v, want the resize to follow the tolerated growpart", got)
	}
}

// Any other growpart failure is a real one, and extending a filesystem onto
// capacity the partition does not have would be the corrupting move.
func TestGrowFilesystem_FailsOnARealGrowpartError(t *testing.T) {
	log := &runLog{errOn: defaultGrowpart, errOut: "unable to read partition table", err: errors.New("exit status 2")}
	g := newTestStorage(t, "/dev/vdb1", "ext4", log.run)
	seedPartition(t, g, "vdb", "vdb1", "1")

	if _, err := g.GrowFilesystem(context.Background()); err == nil {
		t.Fatal("GrowFilesystem succeeded on a failed growpart, want an error")
	}
	if got := log.names(); len(got) != 1 {
		t.Errorf("ran %v, want nothing after the failed growpart", got)
	}
}

// Guessing a tool for an unrecognised filesystem would either do nothing or
// corrupt one this image was never meant to be holding.
func TestGrowFilesystem_RefusesAnUnsupportedFilesystem(t *testing.T) {
	log := &runLog{}
	g := newTestStorage(t, "/dev/vdb", "btrfs", log.run)

	_, err := g.GrowFilesystem(context.Background())
	if err == nil || !strings.Contains(err.Error(), "btrfs") {
		t.Fatalf("err = %v, want a refusal naming the filesystem", err)
	}
	if len(log.calls) != 0 {
		t.Errorf("ran %v against an unsupported filesystem, want nothing", log.names())
	}
}

// Nothing mounted at the data mount is a broken guest rather than a grow to
// attempt against whatever else happens to be there.
func TestGrowFilesystem_FailsWhenTheDataVolumeIsNotMounted(t *testing.T) {
	log := &runLog{}
	g := newTestStorage(t, "", "", log.run)

	if _, err := g.GrowFilesystem(context.Background()); err == nil {
		t.Fatal("GrowFilesystem succeeded with no data volume mounted, want an error")
	}
	if len(log.calls) != 0 {
		t.Errorf("ran %v with nothing mounted, want nothing", log.names())
	}
}

// The control plane's grow-filesystem directive has to reach the guest's
// storage, or a grown volume never becomes usable capacity.
func TestCommandRegistry_GrowFilesystemReachesTheGuestStorage(t *testing.T) {
	storage := &fakeStorage{message: "grew /dev/vdb (ext4) at /var/lib/postgresql"}

	reply := newCommander(nil, newCommandRegistry(&fakeEngine{}, storage), 0).execute(context.Background(),
		handlers_rds.Command{CommandID: "cmd-9", Type: handlers_rds.CommandGrowFilesystem})

	if !storage.grown {
		t.Error("the grow-filesystem command did not reach the guest storage")
	}
	if reply.Status != handlers_rds.CommandStatusSucceeded || reply.Message != storage.message {
		t.Errorf("reply = %+v, want succeeded carrying what the grow reported", reply)
	}
}
