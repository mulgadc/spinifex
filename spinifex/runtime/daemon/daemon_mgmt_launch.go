package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// mgmtLaunchDirName holds each system VM's launch record under the data dir.
const mgmtLaunchDirName = "mgmt-launch"

func (d *Daemon) mgmtLaunchDir() string {
	return filepath.Join(DataDirOrDefault(d.config.DataDir), mgmtLaunchDirName)
}

// mgmtLaunchDeps wires launch identity only when this node's allocator uses
// the durable record; otherwise the cleaner releases addresses as before.
func (d *Daemon) mgmtLaunchDeps() vm.MgmtLaunchDeps {
	if d.mgmtIPAllocator == nil || !d.mgmtIPAllocator.isDurable() {
		return vm.MgmtLaunchDeps{}
	}
	return vm.MgmtLaunchDeps{Dir: d.mgmtLaunchDir(), Addresses: mgmtLaunchAddresses{a: d.mgmtIPAllocator}}
}

var errMgmtLaunchLocked = errors.New("another daemon holds this node's management launch records")

// lockMgmtLaunchDir takes the exclusive lock that keeps a second daemon on
// this state directory from launching or releasing alongside this one. It is
// held until the process exits; it does not fence other hosts.
func (d *Daemon) lockMgmtLaunchDir() error {
	dir := d.mgmtLaunchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create management launch dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open management launch lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("%w: %s", errMgmtLaunchLocked, dir)
		}
		return fmt.Errorf("lock management launch dir: %w", err)
	}
	d.mgmtLaunchLock = f
	return nil
}

// recordNewMgmtLaunch writes a new system VM's first launch record once its
// reservation was created by this allocation. A reservation that already
// existed leaves the record alone, so the launch proves its own history.
func (d *Daemon) recordNewMgmtLaunch(instance *vm.VM, inserted bool) error {
	if !inserted {
		return nil
	}
	return d.vmMgr.InitMgmtLaunch(instance)
}

// releaseUnlaunchedMgmtIP returns an address whose launch never started. A
// durable reservation that predates this launch is kept: nothing here proves
// its earlier process stopped.
func (d *Daemon) releaseUnlaunchedMgmtIP(instanceID string, inserted bool) {
	if inserted || !d.mgmtIPAllocator.isDurable() {
		d.mgmtIPAllocator.Release(instanceID)
	}
}
