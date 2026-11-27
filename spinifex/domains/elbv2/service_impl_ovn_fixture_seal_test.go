package elbv2

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sealTestName = "ovn-seal"

// rawClaim reads the claim outside the wrapped bucket. ok is false when absent.
func (h *ovnCreateHarness) rawClaim(name string) (rev uint64, owner fixtureOwner, ok bool) {
	e, err := h.bucket(KVBucketELBv2).Get(h.t.Context(), LBNameKey(name, testAccountID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, fixtureOwner{}, false
	}
	require.NoError(h.t, err)
	require.NoError(h.t, json.Unmarshal(e.Value(), &owner))
	return e.Revision(), owner, true
}

// rawSlotOwner returns the owner named by a slot, or "" when the slot is absent.
func (h *ovnCreateHarness) rawSlotOwner(key string) string {
	e, err := h.bucket(KVBucketELBv2).Get(h.t.Context(), key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return ""
	}
	require.NoError(h.t, err)
	var slot fixtureSlot
	require.NoError(h.t, json.Unmarshal(e.Value(), &slot))
	return slot.OwnerID
}

// parentsBlocked reports whether the VPC and subnet delete checks refuse.
func (h *ovnCreateHarness) parentsBlocked() (vpc, subnet bool) {
	ctx := h.t.Context()
	vpc, err := h.svc.ovn.owners.vpcBlocksDeletion(ctx, testAccountID, h.vpcID)
	require.NoError(h.t, err)
	subnet, err = h.svc.ovn.owners.subnetBlocksDeletion(ctx, testAccountID, h.subnetID)
	require.NoError(h.t, err)
	return vpc, subnet
}

// refuseSealBySize lowers the bucket stream's message limit below the sealed
// value for one publish, so the server refuses it.
func (h *ovnCreateHarness) refuseSealBySize(got *error) {
	h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, value []byte, update func() (uint64, error)) (uint64, error) {
		ctx := h.t.Context()
		stream, err := h.js.Stream(ctx, "KV_"+KVBucketELBv2)
		require.NoError(h.t, err)
		cfg := stream.CachedInfo().Config
		cfg.MaxMsgSize = int32(len(value) - 1)
		_, err = h.js.UpdateStream(ctx, cfg)
		require.NoError(h.t, err)
		rev, uerr := update()
		cfg.MaxMsgSize = -1
		_, err = h.js.UpdateStream(ctx, cfg)
		require.NoError(h.t, err)
		*got = uerr
		return rev, uerr
	})
}

// requireAbandonedThenRetried asserts a refused seal left no claim or slot,
// frees both parents and that an equal retry seals a new owner.
func requireAbandonedThenRetried(t *testing.T, h *ovnCreateHarness, ops []string, elbBefore map[string]uint64) {
	t.Helper()
	claimKey := LBNameKey(sealTestName, testAccountID)
	update := indexOf(ops, "Update "+claimKey)
	require.GreaterOrEqual(t, update, 0, "the seal was attempted: %v", ops)
	vpcSlot, subnetSlot := vpcSlotKey(testAccountID, h.vpcID), subnetSlotKey(testAccountID, h.subnetID)
	assert.Equal(t, []string{
		"Update " + claimKey, "LeaderGet " + claimKey, // refused seal, then the re-read
		"LeaderGet " + claimKey, "Delete " + claimKey, // exact-revision invalidate
		"LeaderGet " + vpcSlot, "Delete " + vpcSlot, "LeaderGet " + subnetSlot, "Delete " + subnetSlot,
	}, ops[update:], "the claim is invalidated before its slots are cleared")

	_, _, ok := h.rawClaim(sealTestName)
	assert.False(t, ok, "no claim survives a refused seal")
	for _, key := range h.slots(h.subnetID) {
		assert.Empty(t, h.rawSlotOwner(key), "slot %s survives a refused seal", key)
	}
	vpcBlocked, subnetBlocked := h.parentsBlocked()
	assert.False(t, vpcBlocked, "the VPC is eligible for deletion")
	assert.False(t, subnetBlocked, "the subnet is eligible for deletion")
	for k, rev := range elbBefore {
		assert.Equal(t, rev, h.state(KVBucketELBv2)[k], "refused seal changed existing key %s", k)
	}

	lb, err := h.create(h.input(sealTestName, h.subnetID, h.groups[0]))
	require.NoError(t, err, "an immediate equal retry succeeds")
	_, owner, ok := h.rawClaim(sealTestName)
	require.True(t, ok)
	assert.True(t, owner.Sealed)
	assert.Equal(t, owner.Root.LoadBalancerArn, aws.StringValue(lb.LoadBalancerArn))
	for _, key := range h.slots(h.subnetID) {
		assert.Equal(t, owner.OwnerID, h.rawSlotOwner(key), "slot %s names the retry owner", key)
	}
}

func indexOf(ops []string, op string) int {
	for i, o := range ops {
		if o == op {
			return i
		}
	}
	return -1
}

func TestOVNFixtureCreate_ServerRefusedSealReleasesClaimAndSlots(t *testing.T) {
	h := newOVNCreateHarness(t)
	elbBefore := h.state(KVBucketELBv2)
	var uerr error
	h.refuseSealBySize(&uerr)
	h.takeOps()

	_, err := h.create(h.input(sealTestName, h.subnetID, h.groups[0]))
	requireFenceCode(t, err, awserrors.ErrorServerInternal)
	apiErr, ok := errors.AsType[*jetstream.APIError](uerr)
	require.True(t, ok, "the server refused the seal: %v", uerr)
	require.Equal(t, jetstream.JSErrCodeStreamMessageExceedsMaximum, apiErr.ErrorCode)

	requireAbandonedThenRetried(t, h, h.takeOps()[KVBucketELBv2], elbBefore)
}

func TestOVNFixtureCreate_ClientRefusedSealReleasesClaimAndSlots(t *testing.T) {
	h := newOVNCreateHarness(t)
	elbBefore := h.state(KVBucketELBv2)
	h.elbKV.interceptUpdate(KeyPrefixLBName, func(string, []byte, func() (uint64, error)) (uint64, error) {
		return 0, nats.ErrMaxPayload
	})
	h.takeOps()

	_, err := h.create(h.input(sealTestName, h.subnetID, h.groups[0]))
	requireFenceCode(t, err, awserrors.ErrorServerInternal)

	requireAbandonedThenRetried(t, h, h.takeOps()[KVBucketELBv2], elbBefore)
}

func TestOVNFixtureCreate_CommittedSealWithLostResponseAndReadResolvesAfterRestart(t *testing.T) {
	h := newOVNCreateHarness(t)
	readErr := errors.New("simulated claim read loss")
	h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, _ []byte, update func() (uint64, error)) (uint64, error) {
		_, err := update()
		require.NoError(t, err)
		h.elbKV.setFailGetPrefix(KeyPrefixLBName, readErr)
		return 0, nats.ErrTimeout
	})

	_, err := h.create(h.input(sealTestName, h.subnetID, h.groups[0]))
	requireFenceCode(t, err, awserrors.ErrorServerInternal)
	rev, stored, ok := h.rawClaim(sealTestName)
	require.True(t, ok, "a committed seal is kept")
	require.True(t, stored.Sealed)
	for _, key := range h.slots(h.subnetID) {
		assert.Equal(t, stored.OwnerID, h.rawSlotOwner(key), "slot %s is kept", key)
	}
	elbAfter := h.state(KVBucketELBv2)

	h.restart()
	lb, err := h.create(h.input(sealTestName, h.subnetID, h.groups[0]))
	require.NoError(t, err)
	assert.Equal(t, stored.Root.LoadBalancerArn, aws.StringValue(lb.LoadBalancerArn), "the retry returns the durable identity")
	assert.Equal(t, stored.Root.CreatedAt, aws.TimeValue(lb.CreatedTime))
	assert.Equal(t, []string{"LeaderGet " + LBNameKey(sealTestName, testAccountID)}, h.takeOps()[KVBucketELBv2])
	again, _, _ := h.rawClaim(sealTestName)
	assert.Equal(t, rev, again)
	assert.Equal(t, elbAfter, h.state(KVBucketELBv2))
}

func TestOVNFixtureCreate_UncertainSealKeepsPendingClaimAndSlots(t *testing.T) {
	cases := []struct {
		name  string
		moved bool
		hook  func(h *ovnCreateHarness) func(string, []byte, func() (uint64, error)) (uint64, error)
	}{
		{"timeout before publish", false, func(*ovnCreateHarness) func(string, []byte, func() (uint64, error)) (uint64, error) {
			return func(string, []byte, func() (uint64, error)) (uint64, error) { return 0, nats.ErrTimeout }
		}},
		{"other server error", false, func(*ovnCreateHarness) func(string, []byte, func() (uint64, error)) (uint64, error) {
			return func(string, []byte, func() (uint64, error)) (uint64, error) {
				return 0, &jetstream.APIError{Code: 503, ErrorCode: jetstream.JSErrCodeStreamNotFound}
			}
		}},
		{"size refusal after the claim moved", true, func(h *ovnCreateHarness) func(string, []byte, func() (uint64, error)) (uint64, error) {
			return func(key string, _ []byte, _ func() (uint64, error)) (uint64, error) {
				raw := h.bucket(KVBucketELBv2)
				e, err := raw.Get(h.t.Context(), key)
				require.NoError(h.t, err)
				_, err = raw.Update(h.t.Context(), key, e.Value(), e.Revision())
				require.NoError(h.t, err)
				return 0, nats.ErrMaxPayload
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newOVNCreateHarness(t)
			var pendingRev uint64
			hook := tc.hook(h)
			h.elbKV.interceptUpdate(KeyPrefixLBName, func(key string, value []byte, update func() (uint64, error)) (uint64, error) {
				pendingRev, _, _ = h.rawClaim(sealTestName)
				return hook(key, value, update)
			})

			_, err := h.create(h.input(sealTestName, h.subnetID, h.groups[0]))
			requireFenceCode(t, err, awserrors.ErrorServerInternal)
			rev, owner, ok := h.rawClaim(sealTestName)
			require.True(t, ok, "an uncertain seal keeps the claim")
			assert.False(t, owner.Sealed)
			if !tc.moved {
				assert.Equal(t, pendingRev, rev, "the pending claim keeps its revision")
			}
			for _, key := range h.slots(h.subnetID) {
				assert.Equal(t, owner.OwnerID, h.rawSlotOwner(key), "slot %s is kept", key)
			}
			vpcBlocked, subnetBlocked := h.parentsBlocked()
			assert.True(t, vpcBlocked)
			assert.True(t, subnetBlocked)

			_, err = h.create(h.input(sealTestName, h.subnetID, h.groups[0]))
			requireFenceCode(t, err, awserrors.ErrorELBv2DuplicateLoadBalancer)
			kept, _, _ := h.rawClaim(sealTestName)
			assert.Equal(t, rev, kept, "an unexpired pending claim refuses a retry")
		})
	}
}

func TestSealWriteRejected(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nats.ErrMaxPayload, true},
		{&jetstream.APIError{Code: 400, ErrorCode: jetstream.JSErrCodeStreamMessageExceedsMaximum}, true},
		{errors.Join(errors.New("nats"), jetstream.ErrStreamMessageExceedsMaximum), true},
		{nats.ErrTimeout, false},
		{jetstream.ErrKeyExists, false},
		{&jetstream.APIError{Code: 503, ErrorCode: jetstream.JSErrCodeStreamNotFound}, false},
		{errors.New("message size exceeds maximum allowed"), false},
	} {
		assert.Equal(t, tc.want, sealWriteRejected(tc.err), "%v", tc.err)
	}
}
