package nats

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// DefaultStoreReserveBytes is the headroom held back on the JetStream store
// filesystem. Stream recovery rewrites index, meta and tombstone state before
// the server accepts writes, and 256 MiB covers that with margin.
const DefaultStoreReserveBytes int64 = 256 << 20

// DefaultStoreReserveInterval paces the background re-arm. The reserve only
// changes state around a full disk, so sampling is cheap and rare.
const DefaultStoreReserveInterval = 15 * time.Minute

// StoreReserveName is the reserve file. It sits beside the "jetstream"
// directory rather than inside it: nats-server enumerates the contents of
// store_dir/jetstream as accounts, but never the reserve's own parent entries.
const StoreReserveName = ".spinifex-nats-reserve"

// StoreReserve is a preallocated file that keeps the JetStream store
// filesystem off literal zero. nats-server dereferences a nil named return in
// its deferred unlock when recovery cannot write, so a full disk kills it.
type StoreReserve struct {
	// Dir is the configured JetStream store_dir.
	Dir string
	// Size is how many bytes to hold back. Zero or negative disables the reserve.
	Size int64
	// Interval paces Maintain. Zero selects DefaultStoreReserveInterval.
	Interval time.Duration
	// FreeBytes reports bytes available to an unprivileged writer on Dir's
	// filesystem. Nil selects statfs; tests substitute their own probe.
	FreeBytes func(dir string) (uint64, error)
}

// Path returns the reserve file's location.
func (r *StoreReserve) Path() string {
	return filepath.Join(r.Dir, StoreReserveName)
}

// Prepare arms the reserve when the store filesystem has room to spare and
// releases it when the filesystem can no longer supply recovery headroom on
// its own. It reports whether this call released the reserve.
func (r *StoreReserve) Prepare() (bool, error) {
	if r.Size <= 0 || r.Dir == "" {
		return false, nil
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return false, fmt.Errorf("create jetstream store dir %s: %w", r.Dir, err)
	}
	free, err := r.free()
	if err != nil {
		return false, err
	}

	// Arming costs Size, so only arm when what remains still clears Size.
	// A single threshold would arm and release on alternate starts.
	size := uint64(r.Size)
	switch {
	case free < size:
		return r.release()
	case free >= 2*size:
		return false, r.arm()
	default:
		return false, nil
	}
}

// Apply runs Prepare and reports the outcome, logging rather than failing:
// a server with no headroom left must still be allowed to start.
func (r *StoreReserve) Apply() {
	released, err := r.Prepare()
	if err != nil {
		slog.Warn("JetStream store reserve unavailable", "store_dir", r.Dir, "err", err)
		return
	}
	if released {
		slog.Warn("Released the JetStream store reserve, the store filesystem is out of space",
			"store_dir", r.Dir, "reserve_bytes", r.Size)
	}
}

// Maintain re-arms the reserve once the filesystem recovers, so a node that
// spent its headroom is protected again without waiting for the next restart.
// It returns when stop is closed.
func (r *StoreReserve) Maintain(stop <-chan struct{}) {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultStoreReserveInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			r.Apply()
		}
	}
}

// free reports the bytes available on the store filesystem, using the
// injected probe when one is set.
func (r *StoreReserve) free() (uint64, error) {
	if r.FreeBytes != nil {
		return r.FreeBytes(r.Dir)
	}
	return statfsFreeBytes(r.Dir)
}

// arm creates the reserve file at Size, allocating real blocks. It is a no-op
// once the file is already the right size.
func (r *StoreReserve) arm() error {
	f, err := os.OpenFile(r.Path(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open jetstream store reserve: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat jetstream store reserve: %w", err)
	}
	if fi.Size() == r.Size {
		return nil
	}
	// Fallocate, not Truncate: a sparse file reserves no blocks at all, so
	// the guard would hold back nothing while appearing to be armed.
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, r.Size); err != nil {
		return fmt.Errorf("allocate jetstream store reserve: %w", err)
	}
	return nil
}

// release removes the reserve file, handing its blocks back to the
// filesystem so stream recovery has somewhere to write.
func (r *StoreReserve) release() (bool, error) {
	err := os.Remove(r.Path())
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("release jetstream store reserve: %w", err)
	}
}

// statfsFreeBytes reports the space available to an unprivileged writer on the
// filesystem holding dir, which is what a full disk actually leaves JetStream.
func statfsFreeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	if st.Bsize <= 0 {
		return 0, fmt.Errorf("statfs %s: invalid block size %d", dir, st.Bsize)
	}
	return st.Bavail * uint64(st.Bsize), nil
}
