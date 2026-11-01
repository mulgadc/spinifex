package vpc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A replicated bucket can answer a direct get from a replica that has not yet
// applied the caller's own previous write. The next compare-and-swap then
// carries a revision that is already superseded.
func newStaleReadENI(t *testing.T) (*VPCServiceImpl, string, func() ENIRecord) {
	t.Helper()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)
	var inner jetstream.KeyValue
	svc.WrapENIKVForTest(func(kv jetstream.KeyValue) jetstream.KeyValue {
		inner = kv
		return testutil.NewStaleReadKV(kv)
	})
	// Reads the stored record directly: the wrapped bucket would serve the
	// check itself from a lagging replica.
	stored := func() ENIRecord {
		entry, err := inner.Get(context.Background(), kvutil.AccountKey(testAccountID, eniID))
		require.NoError(t, err)
		var rec ENIRecord
		require.NoError(t, json.Unmarshal(entry.Value(), &rec))
		return rec
	}
	return svc, eniID, stored
}

func TestUpdateENIAfterOwnWriteSurvivesStaleReplicaRead(t *testing.T) {
	t.Parallel()
	svc, eniID, stored := newStaleReadENI(t)

	_, err := svc.AttachENI(testAccountID, eniID, "i-test123", 0)
	require.NoError(t, err)

	err = svc.UpdateENI(testAccountID, eniID, func(r *ENIRecord) { r.InstanceOwnerId = "000000000000" })
	require.NoError(t, err, "a read served from a lagging replica must not lose the update")

	rec := stored()
	assert.Equal(t, "000000000000", rec.InstanceOwnerId)
	assert.Equal(t, "i-test123", rec.InstanceId, "the attach must survive the update")
}

func TestAttachENIAfterOwnWriteSurvivesStaleReplicaRead(t *testing.T) {
	t.Parallel()
	svc, eniID, stored := newStaleReadENI(t)

	require.NoError(t, svc.UpdateENI(testAccountID, eniID, func(r *ENIRecord) { r.Description = "managed" }))

	_, err := svc.AttachENI(testAccountID, eniID, "i-test123", 0)
	require.NoError(t, err, "a read served from a lagging replica must not fail the attach")

	rec := stored()
	assert.Equal(t, "in-use", rec.Status)
	assert.Equal(t, "managed", rec.Description)
}

func TestUpdateENIReportsTheStoreError(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)

	cause := errors.New("stream unavailable")
	svc.WrapENIKVForTest(func(kv jetstream.KeyValue) jetstream.KeyValue {
		return testutil.NewRejectingKV(kv, func(string, []byte) error { return cause })
	})

	err := svc.UpdateENI(testAccountID, eniID, func(r *ENIRecord) { r.Description = "x" })
	require.Error(t, err)
	assert.ErrorIs(t, err, cause, "the caller logs this error, so it must name the store failure")
}

func TestAttachENIWithOwnerStampsTheOwnerInTheAttachWrite(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)

	var writes []ENIRecord
	svc.WrapENIKVForTest(func(kv jetstream.KeyValue) jetstream.KeyValue {
		return testutil.NewRejectingKV(kv, func(_ string, value []byte) error {
			var rec ENIRecord
			require.NoError(t, json.Unmarshal(value, &rec))
			writes = append(writes, rec)
			return nil
		})
	})

	_, err := svc.AttachENIWithOwner(testAccountID, eniID, "i-sys", 0, "000000000000")
	require.NoError(t, err)

	require.Len(t, writes, 1, "attach and owner stamp are one write")
	assert.Equal(t, "in-use", writes[0].Status)
	assert.Equal(t, "000000000000", writes[0].InstanceOwnerId)
}

func TestCASENIGivesUpAfterBoundedConflicts(t *testing.T) {
	t.Parallel()
	svc, eniID, _ := newStaleReadENI(t)
	conflicts := 0
	err := svc.casENI(t.Context(), kvutil.AccountKey(testAccountID, eniID), eniID, func(*ENIRecord) error {
		conflicts++
		// Move the record under the next compare-and-swap, as a competing writer does.
		return svc.UpdateENI(testAccountID, eniID, func(r *ENIRecord) { r.Description = "other writer" })
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, jetstream.ErrKeyExists, "the conflict is reported, not hidden")
	assert.Equal(t, eniCASAttempts, conflicts)
}
