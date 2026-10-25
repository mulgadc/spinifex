//test:in-package — drives the unexported startup reconcile retry installOCIAllocators hands an OCI pool to.
package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external/ocinet"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lateKVStore answers every read with the error JetStream gives before the
// bindings KV has a responder, until it has failed `unreachable` times.
type lateKVStore struct {
	*ocinet.MemStore

	mu          sync.Mutex
	unreachable int
	err         error
	gets        int
}

func (s *lateKVStore) Get(ctx context.Context, poolName string) (ocinet.Record, error) {
	s.mu.Lock()
	s.gets++
	if s.gets <= s.unreachable {
		s.mu.Unlock()
		return ocinet.Record{}, fmt.Errorf("kvstore: get %s: %w", poolName, s.err)
	}
	s.mu.Unlock()
	return s.MemStore.Get(ctx, poolName)
}

func (s *lateKVStore) Gets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

const testOCIVNIC = "ocid1.vnic.oc1..vnic1"

// ociPoolWithLeak builds a pool whose VNIC carries a pair an interrupted
// Allocate left behind, the thing only the reconcile ever finds.
func ociPoolWithLeak(t *testing.T, store ocinet.Store) (ociPool, *oci.Fake) {
	t.Helper()
	ctx := context.Background()
	fake := oci.NewFake()
	alloc, err := ocinet.New(fake, store, ocinet.Config{
		Pool:          external.ExternalPoolConfig{Name: "oci-public", Source: external.SourceOCI},
		VNICID:        testOCIVNIC,
		CompartmentID: "ocid1.compartment.oc1..comp1",
	})
	require.NoError(t, err)
	priv, err := fake.AssignPrivateIP(ctx, testOCIVNIC, netip.Addr{}, ocinet.DisplayNamePrefix+"eipalloc-lost")
	require.NoError(t, err)
	_, err = fake.CreatePublicIP(ctx, "ocid1.compartment.oc1..comp1", priv.ID, ocinet.DisplayNamePrefix+"eipalloc-lost")
	require.NoError(t, err)
	return ociPool{name: "oci-public", alloc: alloc}, fake
}

// A node that starts before the bindings KV has a responder must still run its
// startup reconcile once the KV answers, not give up after one attempt.
func TestOCIStartupReconcileRetriesUntilTheBindingsKVAnswers(t *testing.T) {
	store := &lateKVStore{MemStore: ocinet.NewMemStore(), unreachable: 3, err: nats.ErrNoResponders}
	pool, fake := ociPoolWithLeak(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.retryStartupReconcile(ctx, time.Millisecond, time.Millisecond)

	require.NoError(t, ctx.Err(), "retry never saw the KV answer")
	assert.Equal(t, 4, store.Gets(), "three unreachable reads, then the one that answered")
	assert.Empty(t, fake.PrivateIPs(), "leaked private IP was not collected once the KV answered")
	assert.Empty(t, fake.PublicIPs(), "leaked reserved public IP was not collected once the KV answered")
}

// A failure that is not the KV being unreachable is the periodic loop's to
// retry; the startup retry reports it once and stops.
func TestOCIStartupReconcileStopsOnAFailureThatReachedTheKV(t *testing.T) {
	store := &lateKVStore{MemStore: ocinet.NewMemStore()}
	pool, fake := ociPoolWithLeak(t, store)
	fake.FailOp("ListPrivateIPs", errors.New("oci: 500 InternalError"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.retryStartupReconcile(ctx, time.Millisecond, time.Millisecond)

	require.NoError(t, ctx.Err(), "retry kept going on an OCI failure")
	assert.Len(t, fake.PrivateIPs(), 1, "the failed pass must not have collected anything")
}

// The log a reader acts on depends on this split: an unreachable KV says
// nothing about leaks, while a failed pass that read the KV may have left some.
func TestOCIReconcileLogsAnUnreachableKVApartFromAFailedPass(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	unreachable := &lateKVStore{MemStore: ocinet.NewMemStore(), unreachable: 1, err: nats.ErrNoResponders}
	pool, _ := ociPoolWithLeak(t, unreachable)
	require.ErrorIs(t, pool.reconcile(context.Background(), true), nats.ErrNoResponders)
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "bindings KV store not reachable")
	assert.NotContains(t, buf.String(), "leaked addresses may be billing")

	buf.Reset()
	pool, fake := ociPoolWithLeak(t, &lateKVStore{MemStore: ocinet.NewMemStore()})
	fake.FailOp("ListPrivateIPs", errors.New("oci: 500 InternalError"))
	require.Error(t, pool.reconcile(context.Background(), true))
	assert.Contains(t, buf.String(), "level=ERROR")
	assert.Contains(t, buf.String(), "leaked addresses may be billing")
	assert.NotContains(t, buf.String(), "not reachable")
}
