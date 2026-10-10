package viperblocklegacyv1_test

import (
	"encoding/json"
	"testing"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDirtyBucket_Name pins the JetStream KV bucket name. Renaming it without
// a migration would strand every existing dirty marker in an unread bucket.
func TestDirtyBucket_Name(t *testing.T) {
	assert.Equal(t, "VIPERBLOCK_VOLUME_DIRTY", viperblocklegacyv1.DirtyBucket)
}

// TestDirtyRecord_JSON pins the stored JSON shape read across nodes from the
// dirty bucket, including the RFC 3339 encoding of Since.
func TestDirtyRecord_JSON(t *testing.T) {
	since := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	golden := `{"owner":"node-a","generation":7,"since":"2024-01-02T03:04:05Z","reason":"seal volume: predastore unreachable"}`
	record := viperblocklegacyv1.DirtyRecord{
		Owner:      "node-a",
		Generation: 7,
		Since:      since,
		Reason:     "seal volume: predastore unreachable",
	}

	marshaled, err := json.Marshal(record)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded viperblocklegacyv1.DirtyRecord
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.True(t, decoded.Since.Equal(since))
	decoded.Since = since
	assert.Equal(t, record, decoded)
}

// TestVolumeKeyPattern_PinsValidAndInvalidKeys characterizes which volume
// names are accepted as a JetStream KV key, for both the lease and the
// dirty-marker buckets. A name carrying "." or ">" would address somebody
// else's key, which is what the pattern exists to refuse.
func TestVolumeKeyPattern_PinsValidAndInvalidKeys(t *testing.T) {
	valid := []string{"vol-1", "VOL_1", "abcXYZ09", "a", "A-B_c9"}
	for _, key := range valid {
		if !viperblocklegacyv1.VolumeKeyPattern.MatchString(key) {
			t.Errorf("expected %q to be accepted as a volume key", key)
		}
	}

	invalid := []string{"", "vol.1", "vol/1", "vol:1", "vol 1", "vol>1", "ebs.*", "day's"}
	for _, key := range invalid {
		if viperblocklegacyv1.VolumeKeyPattern.MatchString(key) {
			t.Errorf("expected %q to be rejected as a volume key", key)
		}
	}
}
