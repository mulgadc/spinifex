package vpc

import (
	"errors"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A replicated bucket can answer a direct get from a replica that has not yet
// applied the caller's own previous write. The next compare-and-swap then
// carries a revision that is already superseded.
func newStaleReadENI(t *testing.T) (*VPCServiceImpl, string) {
	t.Helper()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)
	svc.WrapENIKVForTest(func(kv jetstream.KeyValue) jetstream.KeyValue { return testutil.NewStaleReadKV(kv) })
	return svc, eniID
}

func TestUpdateENIAfterOwnWriteSurvivesStaleReplicaRead(t *testing.T) {
	t.Parallel()
	svc, eniID := newStaleReadENI(t)

	_, err := svc.AttachENI(testAccountID, eniID, "i-test123", 0)
	require.NoError(t, err)

	err = svc.UpdateENI(testAccountID, eniID, func(r *ENIRecord) { r.InstanceOwnerId = "000000000000" })
	require.NoError(t, err, "a read served from a lagging replica must not lose the update")

	rec, err := svc.GetENIRecord(testAccountID, eniID)
	require.NoError(t, err)
	assert.Equal(t, "000000000000", rec.InstanceOwnerId)
	assert.Equal(t, "i-test123", rec.InstanceId, "the attach must survive the update")
}

func TestAttachENIAfterOwnWriteSurvivesStaleReplicaRead(t *testing.T) {
	t.Parallel()
	svc, eniID := newStaleReadENI(t)

	require.NoError(t, svc.UpdateENI(testAccountID, eniID, func(r *ENIRecord) { r.Description = "managed" }))

	_, err := svc.AttachENI(testAccountID, eniID, "i-test123", 0)
	require.NoError(t, err, "a read served from a lagging replica must not fail the attach")

	rec, err := svc.GetENIRecord(testAccountID, eniID)
	require.NoError(t, err)
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
