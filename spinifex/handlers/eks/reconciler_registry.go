package handlers_eks

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"
)

// ErrLeaseHeld signals the leader lease is held by another node.
// Spawn treats it as benign and retries on the next tick.
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

// SpawnReconcilerFn starts a per-cluster reconciler goroutine; injectable for tests.
type SpawnReconcilerFn func(ctx context.Context, accountID, clusterName string) (release func(), err error)

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

	release, err := spawnFn(ctx, accountID, clusterName)
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
		<-ctx.Done()
		r.mu.Lock()
		rel := h.release
		r.mu.Unlock()
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

// RunClusterReconciler is the production SpawnReconcilerFn: constructs a ClusterReconciler,
// acquires the lease, and drives Run in a goroutine. Returns ErrLeaseHeld if another node wins.
func RunClusterReconciler(
	ctx context.Context,
	leaderKV, acctKV nats.KeyValue,
	accountID, clusterName, holderID, healthURL string,
	opts ...ReconcilerOption,
) (func(), error) {
	r, err := NewClusterReconciler(leaderKV, acctKV, accountID, clusterName, holderID, healthURL, opts...)
	if err != nil {
		return nil, err
	}
	release, ok := r.AcquireLease()
	if !ok {
		slog.Info("RunClusterReconciler: lease held elsewhere, skipping spawn",
			"accountID", accountID, "cluster", clusterName)
		return nil, ErrLeaseHeld
	}
	go func() {
		if runErr := r.Run(ctx); runErr != nil && !errors.Is(runErr, context.Canceled) {
			slog.Info("RunClusterReconciler: Run exited",
				"accountID", accountID, "cluster", clusterName, "err", runErr)
		}
	}()
	return release, nil
}
