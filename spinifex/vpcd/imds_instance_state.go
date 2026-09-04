package vpcd

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// localVMStateReader adapts this node's on-disk instance state to IMDS's
// local-first lookup. vpcd is the composition root for both the network and
// compute planes IMDS straddles, so this adapter — not handlers/imds — is
// what knows the state lives in a daemon-owned file.
type localVMStateReader struct {
	dataDir string

	// readState parses the local state file; overridable in tests to count
	// calls. Defaults to daemon.ReadLocalState.
	readState func(path string) (*daemon.LocalState, error)

	mu     sync.Mutex
	mtime  time.Time
	size   int64
	cached *daemon.LocalState
}

func newLocalVMStateReader(dataDir string) *localVMStateReader {
	return &localVMStateReader{dataDir: dataDir}
}

// LocalVM reads the instance straight off this node's on-disk VM state,
// reusing the last parse while the file's mtime and size are unchanged. IMDS
// serves concurrent requests across per-tap responders, so this is
// mutex-guarded; a stat or parse failure clears the cache and returns the
// error rather than serving a stale hit.
func (r *localVMStateReader) LocalVM(instanceID string) (*vm.VM, error) {
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

// recordLoader mirrors handlers/imds's own narrow instance-record accessor
// interface, so a construction failure below returns a true nil interface
// rather than a *daemon.JetStreamManager typed nil boxed into one.
type recordLoader interface {
	LoadInstanceRecord(instanceID string) (*vm.InstanceRecord, error)
}

// newInstanceRecordLoader opens the shared instance record space, retrying while
// JetStream comes up. Failing here is fatal by design: without it an instance
// this node does not hold locally resolves to nothing, and IMDS serves that as
// "no such instance" — a guest that boots with no key and no error anywhere.
func newInstanceRecordLoader(ctx context.Context, nc *nats.Conn) (recordLoader, error) {
	jsm, err := daemon.NewJetStreamManager(nc, 1)
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
