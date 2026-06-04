package handlers_imds

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestVethStore(t *testing.T) *VethStore {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	subnetVeth, _, err := InitBuckets(js, 1)
	require.NoError(t, err)
	return NewVethStore(subnetVeth)
}

func TestVethStore_PutGetRoundTrip(t *testing.T) {
	s := newTestVethStore(t)
	rec := SubnetVethRecord{
		SubnetID:      "subnet-abc12345",
		ShortSubnetID: "abc12345",
		VPCID:         "vpc-abc12345",
		IMDSPortMAC:   "02:aa:bb:cc:dd:ee",
		SubnetCIDR:    "10.0.1.0/24",
		CreatedAt:     "2026-05-29T00:00:00Z",
	}
	require.NoError(t, s.Put(rec))

	got, err := s.Get("subnet-abc12345")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, rec, *got)
}

func TestVethStore_GetAbsentReturnsNil(t *testing.T) {
	s := newTestVethStore(t)
	got, err := s.Get("subnet-nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestVethStore_PutRejectsEmptySubnetID(t *testing.T) {
	s := newTestVethStore(t)
	require.Error(t, s.Put(SubnetVethRecord{ShortSubnetID: "abc12345"}))
}

func TestVethStore_DeleteIsIdempotent(t *testing.T) {
	s := newTestVethStore(t)
	require.NoError(t, s.Delete("subnet-missing"))

	require.NoError(t, s.Put(SubnetVethRecord{SubnetID: "subnet-1"}))
	require.NoError(t, s.Delete("subnet-1"))
	got, err := s.Get("subnet-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}
