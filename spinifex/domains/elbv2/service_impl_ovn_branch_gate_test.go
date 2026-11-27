package elbv2

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawEntry reads key outside the wrapped bucket, as the stream committed it.
func (h *ovnCreateHarness) rawEntry(key string) jetstream.KeyValueEntry {
	e, err := h.bucket(KVBucketELBv2).Get(h.t.Context(), key)
	require.NoError(h.t, err)
	return e
}

// A deregistration after an acknowledged registration removes the tuple even
// when a lagging replica still answers with the snapshot before it.
func TestOVNFixtureGate_DeregisterAfterStaleOwnerRead(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	a := h.running(1)
	nameKey := LBNameKey("ovn-nlb", testAccountID)
	prior := h.rawEntry(nameKey)
	require.NoError(t, h.register(tgArn, tuple(a, 0)))
	h.elbKV.lag(nameKey, prior)

	require.NoError(t, h.deregister(tgArn, tuple(a, 0)))
	_, owner, ok := h.rawClaim("ovn-nlb")
	require.True(t, ok)
	assert.Equal(t, uint64(4), owner.OwnerRevision, "the deregistration commits one revision after the registration")
	assert.Empty(t, owner.Listener.Targets, "the committed set no longer holds the tuple")
}

// A lost acknowledgement whose snapshot a newer write has superseded is not
// adopted from a lagging replica that still shows the superseded snapshot.
func TestOVNFixtureGate_SupersededUncertainWriteNotAdopted(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	a, b := h.running(1), h.running(2)
	nameKey := LBNameKey("ovn-nlb", testAccountID)
	h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, _ []byte, update func() (uint64, error)) (uint64, error) {
		if _, err := update(); err != nil {
			return 0, err
		}
		superseded := h.rawEntry(nameKey)
		require.NoError(t, h.register(tgArn, tuple(b, 0)))
		h.elbKV.lag(nameKey, superseded)
		return 0, context.DeadlineExceeded
	})

	err := h.register(tgArn, tuple(a, 0))
	requireFenceCode(t, err, awserrors.ErrorServerInternal)
	_, owner, ok := h.rawClaim("ovn-nlb")
	require.True(t, ok)
	assert.Equal(t, uint64(4), owner.OwnerRevision, "the newer write stays as committed")
	assert.Equal(t, []fixtureTarget{{a, 8080}, {b, 8080}}, owner.Listener.Targets)
}

// A replica miss of the subnet slot after seal does not let the subnet go
// while the sealed owner still depends on it.
func TestOVNFixtureGate_DeleteSubnetAfterSlotReplicaMiss(t *testing.T) {
	h := newOVNCreateHarness(t)
	h.sealedOVN("ovn-nlb")
	h.elbKV.lag(subnetSlotKey(testAccountID, h.subnetID), nil)

	_, err := h.vpcs.DeleteSubnet(t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(h.subnetID)}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorDependencyViolation, "the committed subnet slot still blocks")
	assert.NotEmpty(t, h.rawSlotOwner(subnetSlotKey(testAccountID, h.subnetID)), "the slot stays")
}

// A replica miss of the VPC slot does not hide the group binding from a
// registration.
func TestOVNFixtureGate_RegisterAfterVPCSlotReplicaMiss(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	a := h.running(1)
	h.elbKV.lag(vpcSlotKey(testAccountID, h.vpcID), nil)

	require.NoError(t, h.register(tgArn, tuple(a, 0)), "the committed slot binds the group")
	_, owner, ok := h.rawClaim("ovn-nlb")
	require.True(t, ok)
	assert.Equal(t, []fixtureTarget{{a, 8080}}, owner.Listener.Targets)
}

// The second port of an instance that was present on the first attempt is an
// addition on the retry, so the instance is checked again and its stop refuses.
func TestOVNFixtureGate_TwoPortRetryChecksStoppedInstance(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	a := h.running(1)
	require.NoError(t, h.register(tgArn, tuple(a, 0)))
	h.instance.take()
	h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, _ []byte, update func() (uint64, error)) (uint64, error) {
		require.NoError(t, h.deregister(tgArn, tuple(a, 0)))
		h.instance.set(a, func(v *vm.VM) { v.Status = vm.StateStopped })
		return update()
	})

	err := h.register(tgArn, tuple(a, 0), tuple(a, 9000))
	requireFenceCode(t, err, awserrors.ErrorELBv2InvalidTarget)
	_, owner, ok := h.rawClaim("ovn-nlb")
	require.True(t, ok)
	assert.Equal(t, uint64(4), owner.OwnerRevision, "only the competing deregistration commits")
	assert.Empty(t, owner.Listener.Targets)
}

// A registration that would exceed the set limit is TooManyTargets, decided
// before any instance read.
func TestOVNFixtureGate_FullSetIsTooManyTargetsBeforeLookup(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	var full []fixtureTarget
	for n := range fixtureMaxTargets {
		full = append(full, tuple(h.running(n+1), 0))
	}
	require.NoError(t, h.register(tgArn, full...))
	stopped := h.running(99)
	h.instance.set(stopped, func(v *vm.VM) { v.Status = vm.StateStopped })
	h.instance.take()
	before := h.state(KVBucketELBv2)

	requireFenceCode(t, h.register(tgArn, tuple(stopped, 0)), "TooManyTargets")
	assert.Empty(t, h.instance.take(), "the limit is decided before an instance read")
	assert.Equal(t, before, h.state(KVBucketELBv2))
}

// A stored tuple that registration would reject makes the row malformed, so a
// valid edit through the service writes nothing.
func TestOVNFixtureGate_MalformedStoredTupleRefusesEdit(t *testing.T) {
	h := newOVNCreateHarness(t)
	tgArn := h.bound()
	nameKey := LBNameKey("ovn-nlb", testAccountID)
	_, owner, ok := h.rawClaim("ovn-nlb")
	require.True(t, ok)
	owner.Listener.Targets = []fixtureTarget{{InstanceID: "i-0a", Port: 8080}}
	raw, err := json.Marshal(owner)
	require.NoError(t, err)
	_, err = h.bucket(KVBucketELBv2).Put(t.Context(), nameKey, raw)
	require.NoError(t, err)
	b := h.running(2)
	before := h.state(KVBucketELBv2)

	_, err = h.svc.RegisterTargets(t.Context(), &elbv2.RegisterTargetsInput{TargetGroupArn: aws.String(tgArn), Targets: descs(tuple(b, 0))}, testAccountID)
	requireFenceCode(t, err, awserrors.ErrorServerInternal)
	assert.Equal(t, before, h.state(KVBucketELBv2), "a malformed row is not edited")
}

// A stored claim key that is not one literal subject is unreadable, so the
// subnet delete fails closed and the parent and both slots stay. No stream read
// is sent with the invalid selector.
func TestOVNFixtureGate_DeleteSubnetRefusesNonLiteralClaimKey(t *testing.T) {
	for name, suffix := range map[string]string{"wildcard": ".>", "token wildcard": "*", "empty token": "..x"} {
		t.Run(name, func(t *testing.T) {
			h := newOVNCreateHarness(t)
			h.sealedOVN("ovn-nlb")
			key := subnetSlotKey(testAccountID, h.subnetID)
			var slot fixtureSlot
			require.NoError(t, json.Unmarshal(h.rawEntry(key).Value(), &slot))
			slot.ClaimKey = LBNameKey("ovn-nlb", testAccountID) + suffix
			raw, err := json.Marshal(slot)
			require.NoError(t, err)
			_, err = h.bucket(KVBucketELBv2).Put(t.Context(), key, raw)
			require.NoError(t, err)
			before := h.state(KVBucketELBv2)
			reads, err := h.nc.SubscribeSync("$JS.API.STREAM.MSG.GET.>")
			require.NoError(t, err)

			_, err = h.vpcs.DeleteSubnet(t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(h.subnetID)}, testAccountID)
			require.Error(t, err, "an unreadable dependency blocks")
			// The flush round trip delivers every earlier request to reads first.
			require.NoError(t, h.nc.Flush())
			n, _, err := reads.Pending()
			require.NoError(t, err)
			require.NotZero(t, n, "the owner reads go to the stream leader")
			for range n {
				msg, err := reads.NextMsg(time.Second)
				require.NoError(t, err)
				assert.NotContains(t, string(msg.Data), slot.ClaimKey, "the invalid selector reaches no stream read")
			}
			require.NoError(t, reads.Unsubscribe())
			assert.Equal(t, before, h.state(KVBucketELBv2), "both slots and the owner stay as committed")
			_, err = h.bucket(ec2vpc.KVBucketSubnets).Get(t.Context(), kvstore.AccountKey(testAccountID, h.subnetID))
			require.NoError(t, err, "the subnet stays")
		})
	}
}
