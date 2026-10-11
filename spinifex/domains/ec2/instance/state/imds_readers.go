// Package state provides EC2-domain readers for persisted instance state.
// Today it holds only the vpcd IMDS readers, which wrap the daemon's local-file
// reader and instance-record bucket opener unchanged.
package state

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/guestmetadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
)

// LocalVMReader adapts a node's on-disk instance state to IMDS's
// local-first lookup. The composition root wires it with that node's data
// directory into vpcd's guestmetadata construction.
type LocalVMReader struct {
	dataDir string

	// readState parses the local state file; overridable in tests to count
	// calls. Defaults to daemon.ReadLocalState.
	readState func(path string) (*daemon.LocalState, error)

	mu     sync.Mutex
	mtime  time.Time
	size   int64
	cached *daemon.LocalState
}

var _ guestmetadata.LocalStateReader = (*LocalVMReader)(nil)

// NewLocalVMReader builds a reader over dataDir, this node's data directory.
func NewLocalVMReader(dataDir string) *LocalVMReader {
	return &LocalVMReader{dataDir: dataDir}
}

// LocalVM reads the instance straight off this node's on-disk VM state,
// reusing the last parse while mtime and size are unchanged; concurrent
// callers are mutex-guarded, and a stat or parse failure clears the cache.
func (r *LocalVMReader) LocalVM(instanceID string) (*vm.VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	path := daemon.LocalStatePath(r.dataDir)
	info, err := os.Stat(path)
	if err != nil {
		r.cached = nil
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	if r.cached == nil || !info.ModTime().Equal(r.mtime) || info.Size() != r.size {
		read := r.readState
		if read == nil {
			read = daemon.ReadLocalState
		}
		state, readErr := read(path)
		if readErr != nil {
			r.cached = nil
			return nil, readErr
		}
		r.cached = state
		r.mtime = info.ModTime()
		r.size = info.Size()
	}

	if r.cached == nil {
		return nil, nil
	}
	return r.cached.VMS[instanceID], nil
}

// OpenInstanceRecordLoader opens the shared instance record space, retrying
// while JetStream comes up. A failure here is fatal by design: without it an
// unresolved instance serves IMDS "no such instance" with no error anywhere.
func OpenInstanceRecordLoader(ctx context.Context, nc *nats.Conn) (guestmetadata.RecordLoader, error) {
	jsm, err := daemon.NewJetStreamManager(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream manager: %w", err)
	}
	if _, err := kvstore.OpenWithRetry(ctx, daemon.InstanceStateBucket, kvstore.DefaultOpenWindow,
		func(context.Context) (struct{}, error) { return struct{}{}, jsm.InitKVBucket() },
	); err != nil {
		return nil, err
	}
	return jsm, nil
}
