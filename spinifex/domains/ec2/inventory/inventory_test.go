package inventory

import (
	"encoding/json"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListAccountInstancesRefusesAnUnknownNodeSet(t *testing.T) {
	out, err := NewNATSInstances(nil, nil).ListAccountInstances(t.Context(), "000000000000")
	require.Error(t, err)
	assert.Nil(t, out, "an unknown node set must not yield a list")
}

func TestENITagReaderSeparatesGoneFromUnreadable(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	read := NewENITagReader(js, "000000000000")

	_, _, err := read(t.Context(), "eni-1")
	require.Error(t, err, "a missing ENI bucket is a read failure, not an absent ENI")

	enis, err := kvutil.GetOrCreateBucket(t.Context(), js, ec2vpc.KVBucketENIs, 10)
	require.NoError(t, err)
	good, err := json.Marshal(ec2vpc.ENIRecord{NetworkInterfaceId: "eni-good", Tags: map[string]string{"k": "v"}})
	require.NoError(t, err)
	_, err = enis.Put(t.Context(), kvutil.AccountKey("000000000000", "eni-good"), good)
	require.NoError(t, err)
	_, err = enis.Put(t.Context(), kvutil.AccountKey("000000000000", "eni-corrupt"), []byte("{not json"))
	require.NoError(t, err)

	tags, found, err := read(t.Context(), "eni-good")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, map[string]string{"k": "v"}, tags)

	_, found, err = read(t.Context(), "eni-absent")
	require.NoError(t, err)
	assert.False(t, found, "a missing key is the ENI gone")

	_, found, err = read(t.Context(), "eni-corrupt")
	require.Error(t, err, "an undecodable record is a read failure")
	assert.False(t, found)
}
