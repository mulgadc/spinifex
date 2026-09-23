package ocinet_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/external/ocinet"
)

func seedBinding(t *testing.T, s *ocinet.MemStore, pool, public, private string) {
	t.Helper()
	require.NoError(t, s.Mutate(context.Background(), pool, func(rec *ocinet.Record) (bool, error) {
		rec.Bindings[public] = ocinet.Binding{PrivateAddr: private}
		return true, nil
	}))
}

func TestLookupReturnsTheOnWirePrivateAddress(t *testing.T) {
	store := ocinet.NewMemStore()
	seedBinding(t, store, "oci-pool", "203.0.113.9", "10.200.0.183")

	got, err := ocinet.NewLookup(store, []string{"oci-pool"}).DatapathIP(context.Background(), "203.0.113.9")
	require.NoError(t, err)
	assert.Equal(t, "10.200.0.183", got)
}

// vpcd cannot tell which pool an address came from, so an address no OCI pool
// knows about is its own datapath address — every static and DHCP one is.
func TestLookupPassesThroughAnUnboundAddress(t *testing.T) {
	store := ocinet.NewMemStore()
	seedBinding(t, store, "oci-pool", "203.0.113.9", "10.200.0.183")

	got, err := ocinet.NewLookup(store, []string{"oci-pool"}).DatapathIP(context.Background(), "72.52.77.240")
	require.NoError(t, err)
	assert.Equal(t, "72.52.77.240", got)
}

func TestLookupSearchesEveryConfiguredPool(t *testing.T) {
	store := ocinet.NewMemStore()
	seedBinding(t, store, "pool-b", "203.0.113.9", "10.200.0.183")

	got, err := ocinet.NewLookup(store, []string{"pool-a", "pool-b"}).DatapathIP(context.Background(), "203.0.113.9")
	require.NoError(t, err)
	assert.Equal(t, "10.200.0.183", got)
}

// Passing the public address through on a read failure installs a rule that
// looks healthy and blackholes the instance.
func TestLookupStoreFailureIsAnError(t *testing.T) {
	got, err := ocinet.NewLookup(failingStore{}, []string{"oci-pool"}).DatapathIP(context.Background(), "203.0.113.9")
	require.Error(t, err)
	assert.Empty(t, got)
}

type failingStore struct{}

func (failingStore) Mutate(context.Context, string, func(*ocinet.Record) (bool, error)) error {
	return errors.New("unavailable")
}

func (failingStore) Get(context.Context, string) (ocinet.Record, error) {
	return ocinet.Record{}, errors.New("unavailable")
}
