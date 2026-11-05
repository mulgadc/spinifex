package cluster

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrLeaseHeld signals the leader lease is held by another node, and Spawn
// treats it as benign. RunClusterReconciler never returns it: it keeps contending.
var ErrLeaseHeld = errors.New("eks: reconciler lease held by another node")

// ReconcilerRegistry tracks active per-cluster reconciler goroutines; one entry per (accountID, clusterName).
type ReconcilerRegistry struct {
	mu      sync.Mutex
	holders map[string]*reconcilerHandle
}

type reconcilerHandle struct {
	cancel  context.CancelFunc
	release func()
	done    chan struct{}
}

// NewReconcilerRegistry returns an empty registry.
func NewReconcilerRegistry() *ReconcilerRegistry {
	return &ReconcilerRegistry{holders: map[string]*reconcilerHandle{}}
}

// SpawnReconcilerFn starts a per-cluster reconciler goroutine; injectable for
// tests. finished closes when that goroutine exits on its own (terminal cluster
// state or a lost lease), which has no external canceller — Spawn watches it so
// a self-exiting reconciler drops its registry entry instead of leaking it.
type SpawnReconcilerFn func(ctx context.Context, accountID, clusterName string) (release func(), finished <-chan struct{}, err error)

// Spawn launches one reconciler goroutine for (accountID, clusterName), or no-ops if already running.
func (r *ReconcilerRegistry) Spawn(parent context.Context, accountID, clusterName string, spawnFn SpawnReconcilerFn) error {
	if accountID == "" || clusterName == "" {
		return errors.New("eks: ReconcilerRegistry.Spawn empty ids")
	}
	if spawnFn == nil {
		return errors.New("eks: ReconcilerRegistry.Spawn nil SpawnFn")
	}
	key := registryKey(accountID, clusterName)

	r.mu.Lock()
	if _, ok := r.holders[key]; ok {
		r.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	h := &reconcilerHandle{cancel: cancel, done: done}
	r.holders[key] = h
	r.mu.Unlock()

	release, finished, err := spawnFn(ctx, accountID, clusterName)
	if err != nil {
		r.mu.Lock()
		delete(r.holders, key)
		r.mu.Unlock()
		cancel()
		close(done)
		if errors.Is(err, ErrLeaseHeld) {
			return nil
		}
		return err
	}

	r.mu.Lock()
	h.release = release
	r.mu.Unlock()

	go func() {
		// Clean up on whichever comes first: an external Stop/StopAll (ctx
		// cancel) or the reconciler's own Run exiting (finished). Run self-exits
		// on a terminal cluster state or a lost lease with nothing to cancel the
		// ctx, so without watching finished the entry would leak and a same-name
		// recreate would silently no-op at the head of Spawn. A nil finished
		// (test fakes that never exit) just never selects — old ctx-only behaviour.
		select {
		case <-ctx.Done():
		case <-finished:
		}
		r.mu.Lock()
		if r.holders[key] == h {
			delete(r.holders, key)
		}
		rel := h.release
		r.mu.Unlock()
		cancel()
		if rel != nil {
			rel()
		}
		close(done)
	}()

	slog.Info("ReconcilerRegistry: spawned", "accountID", accountID, "cluster", clusterName)
	return nil
}

// Stop cancels a single reconciler. Blocks until the goroutine has exited.
// No-op if no reconciler is registered for the key.
func (r *ReconcilerRegistry) Stop(accountID, clusterName string) {
	key := registryKey(accountID, clusterName)
	r.mu.Lock()
	h, ok := r.holders[key]
	if ok {
		delete(r.holders, key)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	h.cancel()
	<-h.done
	slog.Info("ReconcilerRegistry: stopped", "accountID", accountID, "cluster", clusterName)
}

// StopAll cancels every active reconciler and blocks until all goroutines
// have exited. Called from daemon graceful-shutdown.
func (r *ReconcilerRegistry) StopAll() {
	r.mu.Lock()
	handles := make([]*reconcilerHandle, 0, len(r.holders))
	for k, h := range r.holders {
		handles = append(handles, h)
		delete(r.holders, k)
	}
	r.mu.Unlock()

	for _, h := range handles {
		h.cancel()
	}
	for _, h := range handles {
		<-h.done
	}
}

// Has reports whether a reconciler is currently registered for the key.
func (r *ReconcilerRegistry) Has(accountID, clusterName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.holders[registryKey(accountID, clusterName)]
	return ok
}

func registryKey(accountID, clusterName string) string {
	return accountID + "/" + clusterName
}

// RunClusterReconciler contends for the cluster's lease and drives Run while it
// holds it, in a goroutine, until ctx ends or the cluster leaves CREATING and
// ACTIVE. A held lease is asked for again every ttl/3, so a dead holder's key is
// taken over once it has sat a TTL, and a lost one is contended for again.
// release stops the goroutine and returns once the lease is unlocked.
func RunClusterReconciler(
	ctx context.Context,
	lease *kvsync.Lease, acctKV jetstream.KeyValue,
	accountID, clusterName, healthURL string,
	opts ...ReconcilerOption,
) (func(), <-chan struct{}, error) {
	newRun := func() (*ClusterReconciler, error) {
		return NewClusterReconciler(lease, acctKV, accountID, clusterName, healthURL, opts...)
	}
	if _, err := newRun(); err != nil {
		return nil, nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			// A fresh reconciler per holding: a finished Run keeps its exit reason.
			r, _ := newRun()
			if release, ok := r.AcquireLease(runCtx); ok {
				err := r.Run(runCtx)
				release()
				if !errors.Is(err, ErrReconcilerLeaseLost) {
					if err != nil && !errors.Is(err, context.Canceled) {
						slog.Info("RunClusterReconciler: Run exited",
							"accountID", accountID, "cluster", clusterName, "err", err)
					}
					return
				}
				slog.Info("RunClusterReconciler: lease lost, contending again",
					"accountID", accountID, "cluster", clusterName)
			}
			if !clusterContended(runCtx, acctKV, clusterName) {
				return
			}
			select {
			case <-runCtx.Done():
				return
			case <-time.After(lease.TTL() / 3):
			}
		}
	}()
	return func() { cancel(); <-finished }, finished, nil
}

// clusterContended reports whether the cluster is still one a reconciler is
// wanted for. A read that fails is not evidence it is gone, so it keeps trying.
func clusterContended(ctx context.Context, acctKV jetstream.KeyValue, clusterName string) bool {
	meta, err := GetClusterMeta(ctx, acctKV, clusterName)
	switch {
	case errors.Is(err, ErrClusterNotFound):
		return false
	case err != nil:
		return ctx.Err() == nil
	default:
		return meta.Status == ClusterStatusCreating || meta.Status == ClusterStatusActive
	}
}
