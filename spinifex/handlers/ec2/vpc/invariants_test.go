package handlers_ec2_vpc

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// IMDS-datapath invariant.

// TestI4_ENIIndexBucketShape asserts the eni-by-vpc-ip reverse index keeps its
// contract: keys parse as "vpcID/ip", values carry only immutable identity
// fields (eni_id + account_id), and no mutable/denormalised fields are present.
func TestI4_ENIIndexBucketShape(t *testing.T) {
	_, nc := setupTestVPCServiceWithNC(t)
	kv := openTestENIByIPBucket(t, nc)
	idx := NewENIByIPIndex(kv)

	// Seed a couple of entries through the real writer.
	require.NoError(t, idx.Put("vpc-aaaaaaaa", "10.0.1.5", "eni-aaa", "111122223333"))
	require.NoError(t, idx.Put("vpc-bbbbbbbb", "10.0.2.9", "eni-bbb", "444455556666"))

	// allowedFields is the closed set the value JSON may contain; any new field
	// must be immutable identity, not cached mutable state.
	allowedFields := map[string]struct{}{"eni_id": {}, "account_id": {}}

	keys, err := kv.Keys()
	require.NoError(t, err)
	require.NotEmpty(t, keys)

	for _, key := range keys {
		// Skip migration framework metadata (e.g. _version).
		if strings.HasPrefix(key, "_") {
			continue
		}
		vpcID, ip, ok := strings.Cut(key, "/")
		require.Truef(t, ok, "key %q does not parse as vpcID/ip", key)
		assert.NotEmptyf(t, vpcID, "key %q has empty vpcID", key)
		assert.NotNilf(t, net.ParseIP(ip), "key %q has invalid IP %q", key, ip)

		entry, err := kv.Get(key)
		require.NoError(t, err)

		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(entry.Value(), &raw))

		var v struct {
			ENIId string `json:"eni_id"`
		}
		require.NoError(t, json.Unmarshal(entry.Value(), &v))
		assert.NotEmptyf(t, v.ENIId, "index entry %q has empty eni_id", key)

		for field := range raw {
			_, allowed := allowedFields[field]
			assert.Truef(t, allowed, "index entry %q carries unexpected field %q: the "+
				"eni-by-vpc-ip index is IP→ENI identity only — denormalised mutable "+
				"fields reintroduce the staleness class the design removed", key, field)
		}
	}
}
