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
	vpcVeth, _, err := InitBuckets(js, 1)
	require.NoError(t, err)
	return NewVethStore(vpcVeth)
}

func TestVethStore_PutGetRoundTrip(t *testing.T) {
	s := newTestVethStore(t)
	rec := VPCVethRecord{
		VPCID:       "vpc-abc12345",
		ShortVPCID:  "abc12345",
		IMDSPortMAC: "02:aa:bb:cc:dd:ee",
		LRPNetwork:  "169.254.169.253/30",
		CreatedAt:   "2026-05-29T00:00:00Z",
	}
	require.NoError(t, s.Put(rec))

	got, err := s.Get("vpc-abc12345")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, rec, *got)
}

func TestVethStore_GetAbsentReturnsNil(t *testing.T) {
	s := newTestVethStore(t)
	got, err := s.Get("vpc-nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestVethStore_PutRejectsEmptyVPCID(t *testing.T) {
	s := newTestVethStore(t)
	require.Error(t, s.Put(VPCVethRecord{ShortVPCID: "abc12345"}))
}

func TestVethStore_DeleteIsIdempotent(t *testing.T) {
	s := newTestVethStore(t)
	require.NoError(t, s.Delete("vpc-missing"))

	require.NoError(t, s.Put(VPCVethRecord{VPCID: "vpc-1"}))
	require.NoError(t, s.Delete("vpc-1"))
	got, err := s.Get("vpc-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}
