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

// MgmtLaunchDir is where a node keeps its system VMs' launch records.
func MgmtLaunchDir(dataDir string) string {
	return filepath.Join(DataDirOrDefault(dataDir), mgmtLaunchDirName)
}

func (d *Daemon) mgmtLaunchDir() string { return MgmtLaunchDir(d.config.DataDir) }

// mgmtLaunchDeps wires launch identity unless legacy mode is configured, so a
// missing allocator or unknown mode leaves no address record and every launch
// refuses. In legacy mode the cleaner releases addresses as before.
func (d *Daemon) mgmtLaunchDeps() vm.MgmtLaunchDeps {
	if d.mgmtIPAllocator != nil && d.mgmtIPAllocator.isDurable() {
		return vm.MgmtLaunchDeps{Dir: d.mgmtLaunchDir(), Addresses: mgmtLaunchAddresses{a: d.mgmtIPAllocator}}
	}
	if mode := d.mgmtIPAMMode(); mode == "" || mode == MgmtIPAMLegacy {
		return vm.MgmtLaunchDeps{}
	}
	return vm.MgmtLaunchDeps{Dir: d.mgmtLaunchDir()}
}

// ErrMgmtLaunchLocked reports that another process holds the launch records.
var ErrMgmtLaunchLocked = errors.New("another daemon holds this node's management launch records")

// lockMgmtLaunchDir takes the exclusive lock that keeps a second daemon on
// this state directory from launching or releasing alongside this one. It is
// held until the process exits; it does not fence other hosts.
func (d *Daemon) lockMgmtLaunchDir() error {
	f, err := LockMgmtLaunchDir(d.mgmtLaunchDir())
	if err != nil {
		return err
	}
	d.mgmtLaunchLock = f
	return nil
}

// LockMgmtLaunchDir takes the launch records' exclusive lock without waiting.
// The lock lasts until the returned file is closed.
func LockMgmtLaunchDir(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create management launch dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open management launch lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrMgmtLaunchLocked, dir)
		}
		return nil, fmt.Errorf("lock management launch dir: %w", err)
	}
	return f, nil
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
