package vpcpeering

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreate_ReturnsInitiatingOverStoredPending(t *testing.T) {
	f := newFixture(t)
	pcx := f.create(t, vpcA, vpcB)

	assert.Equal(t, StateInitiatingRequest, aws.StringValue(pcx.Status.Code))
	assert.Equal(t, "Initiating Request to "+testAccount, aws.StringValue(pcx.Status.Message))
	assert.NotNil(t, pcx.ExpirationTime)
	assert.NotNil(t, pcx.RequesterVpcInfo.PeeringOptions, "a request carries the requester's options")
	assert.Nil(t, pcx.AccepterVpcInfo.CidrBlock, "a request names only the accepter's identity")
	assert.NotNil(t, pcx.Tags, "Tags is always present")

	conns, err := f.describe(t, *pcx.VpcPeeringConnectionId)
	require.NoError(t, err)
	require.Len(t, conns, 1)
	assert.Equal(t, StatePendingAcceptance, aws.StringValue(conns[0].Status.Code))
	assert.Equal(t, "Pending Acceptance by "+testAccount, aws.StringValue(conns[0].Status.Message))
}

func TestCreate_SynchronousRefusals(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name  string
		input *ec2.CreateVpcPeeringConnectionInput
		code  string
		msg   string
	}{
		{"missing peer", &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpcA)},
			awserrors.ErrorMissingParameter, "The request must contain the parameter peerVpcId"},
		{"malformed peer", &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpcA), PeerVpcId: aws.String("vpc-bogus")},
			awserrors.ErrorInvalidVpcIdMalformed, `Invalid id: "vpc-bogus" (expecting "vpc-...")`},
		{"unknown requester", &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpcID(99)), PeerVpcId: aws.String(vpcB)},
			awserrors.ErrorInvalidVpcIdMalformed, "The vpc ID '" + vpcID(99) + "' is malformed"},
		{"self", &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcA)},
			awserrors.ErrorInvalidParameterValue, "Invalid value '" + vpcA + "' for peerVpcId. The value of peerVpcId needs to be different to vpcId."},
		{"unknown peer", &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcID(98))},
			awserrors.ErrorInvalidVpcIDNotFound, "The vpc ID '" + vpcID(98) + "' does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.CreateVpcPeeringConnection(t.Context(), tc.input, testAccount)
			requireMessage(t, err, tc.code, tc.msg)
		})
	}

	t.Run("another Region", func(t *testing.T) {
		_, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{
			VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcB), PeerRegion: aws.String("us-east-1"),
		}, testAccount)
		requireCode(t, err, awserrors.ErrorInvalidParameterValue)
	})
	t.Run("the formation's Region", func(t *testing.T) {
		_, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{
			VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcB), PeerRegion: aws.String(testRegion),
		}, testAccount)
		require.NoError(t, err)
	})
	t.Run("another account", func(t *testing.T) {
		_, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{
			VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcC), PeerOwnerId: aws.String("210987654321"),
		}, testAccount)
		requireCode(t, err, awserrors.ErrorInvalidParameterValue)
	})
}

func TestCreate_RepeatedRequestReturnsExistingWithNoWrite(t *testing.T) {
	f := newFixture(t)
	first := f.create(t, vpcA, vpcB)
	id := *first.VpcPeeringConnectionId
	before := f.liveKeys(t)

	again := f.create(t, vpcA, vpcB)
	assert.Equal(t, id, *again.VpcPeeringConnectionId)
	assert.Equal(t, StatePendingAcceptance, *again.Status.Code)
	assert.Equal(t, before, f.liveKeys(t), "a repeated request writes nothing")

	f.activate(t, id)
	again = f.create(t, vpcA, vpcB)
	assert.Equal(t, id, *again.VpcPeeringConnectionId)
	assert.Equal(t, StateActive, *again.Status.Code)

	reversed := f.create(t, vpcB, vpcA)
	assert.NotEqual(t, id, *reversed.VpcPeeringConnectionId, "a reversed request is another direction")
}

func TestCreate_ConcurrentSameDirectionAnswerOneID(t *testing.T) {
	f := newFixture(t)
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			out, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcB)}, testAccount)
			if assert.NoError(t, err) {
				ids[i] = *out.VpcPeeringConnection.VpcPeeringConnectionId
			}
		})
	}
	wg.Wait()
	for _, id := range ids {
		assert.Equal(t, ids[0], id, "every concurrent create answers the winner")
	}
	records := 0
	for k := range f.liveKeys(t) {
		if strings.HasPrefix(k, "pcx.") {
			records++
		}
	}
	assert.Equal(t, 1, records, "each loser deletes its own record")
}

func TestCreate_FailedGoesOnlyToTerminalBucket(t *testing.T) {
	f := newFixture(t)
	f.putVPC(t, testAccount, vpcID(10), "10.1.128.0/17")
	before := f.liveKeys(t)

	var last string
	for range 1000 {
		out, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcID(10))}, testAccount)
		require.NoError(t, err)
		assert.Equal(t, StateInitiatingRequest, *out.VpcPeeringConnection.Status.Code)
		assert.Nil(t, out.VpcPeeringConnection.RequesterVpcInfo.PeeringOptions, "AWS omits the requester options for an overlap")
		last = *out.VpcPeeringConnection.VpcPeeringConnectionId
	}
	assert.Equal(t, before, f.liveKeys(t), "failed creates leave the live bucket and every VPC index unchanged")

	assert.Equal(t, StateFailed, f.status(t, last))
	f.clock.advance(terminalShortWindow)
	assert.Empty(t, f.status(t, last), "a failed connection is visible for 2 hours")
}

func TestCreate_DifferentAZHomesFail(t *testing.T) {
	f := newFixture(t)
	f.updateVPC(t, testAccount, vpcA, func(r *ec2vpc.VPCRecord) { r.AZ = "az1" })
	f.updateVPC(t, testAccount, vpcB, func(r *ec2vpc.VPCRecord) { r.AZ = "az2" })
	pcx := f.create(t, vpcA, vpcB)
	assert.Equal(t, StateFailed, f.status(t, *pcx.VpcPeeringConnectionId))
}

func TestAccept_ProvisioningThenActive(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId

	out, err := f.accept(t, id)
	require.NoError(t, err)
	assert.Equal(t, StateProvisioning, *out.Status.Code)
	assert.Equal(t, "Provisioning", *out.Status.Message)
	assert.Nil(t, out.ExpirationTime)
	assert.NotNil(t, out.AccepterVpcInfo.CidrBlock)
	assert.Equal(t, StateProvisioning, f.status(t, id), "only the reconciler makes it active")

	rec := f.liveRecord(t, id)
	req, acc, err := LinkAddresses(rec.LinkPrefix)
	require.NoError(t, err)
	assert.True(t, linkRange.Contains(req) && linkRange.Contains(acc))

	again, err := f.accept(t, id)
	require.NoError(t, err)
	assert.Equal(t, StateProvisioning, *again.Status.Code)
	assert.Equal(t, rec.Generation, f.liveRecord(t, id).Generation, "a redelivered accept writes nothing")
	assert.Equal(t, 1, countPrefix(f.liveKeys(t), "link."), "a redelivered accept claims no second link")

	require.NoError(t, f.svc.MarkActive(t.Context(), id, rec.Generation))
	assert.Equal(t, StateActive, f.status(t, id))
}

func TestMarkActive_GenerationFence(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	_, err := f.accept(t, id)
	require.NoError(t, err)
	gen := f.liveRecord(t, id).Generation

	require.NoError(t, f.del(t, id))
	require.NoError(t, f.svc.MarkActive(t.Context(), id, gen))
	assert.Equal(t, StateDeleted, f.status(t, id), "a confirmation after delete never resurrects the connection")

	require.NoError(t, f.svc.MarkActive(t.Context(), id, gen-1))
	assert.Equal(t, StateDeleted, f.liveRecord(t, id).Status)
}

func TestDelete_KeepsRecordUntilMarkCleaned(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	f.activate(t, id)

	require.NoError(t, f.del(t, id))
	rec := f.liveRecord(t, id)
	require.NotNil(t, rec, "the record carries the cleanup obligation")
	assert.True(t, rec.CleanupPending)
	assert.Equal(t, StateDeleted, f.status(t, id))
	assert.False(t, f.has(t, pairKey(vpcA, vpcB)), "delete releases the pair claim")
	assert.Equal(t, 1, countPrefix(f.liveKeys(t), "link."), "the link address stays claimed until cleanup")

	require.NoError(t, f.del(t, id), "deleting a deleted connection succeeds again")

	require.NoError(t, f.svc.MarkCleaned(t.Context(), id))
	assert.Nil(t, f.liveRecord(t, id))
	assert.Equal(t, StateDeleted, f.termRecord(t, id).Status)
	assert.Empty(t, f.liveKeys(t), "cleanup releases every claim and index key")
	assert.Equal(t, StateDeleted, f.status(t, id))
	require.NoError(t, f.del(t, id))

	require.NoError(t, f.svc.MarkCleaned(t.Context(), id), "a repeated cleanup is a no-op")
}

func TestDelete_PendingMovesStraightToTerminal(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	require.NoError(t, f.del(t, id))
	assert.Empty(t, f.liveKeys(t))
	assert.Equal(t, "Deleted by "+testAccount, f.termRecord(t, id).StatusMessage)

	f.clock.advance(terminalShortWindow - time.Minute)
	assert.Equal(t, StateDeleted, f.status(t, id))
	f.clock.advance(time.Minute)
	assert.Empty(t, f.status(t, id), "within one account a deleted connection is visible for 2 hours")
}

func TestStateTransitions(t *testing.T) {
	f := newFixture(t)

	rejected := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	require.NoError(t, f.reject(t, rejected))
	assert.Equal(t, StateRejected, f.status(t, rejected))
	assert.Empty(t, f.liveKeys(t))

	failedVpc := vpcID(20)
	f.putVPC(t, testAccount, failedVpc, "10.1.0.0/24")
	failed := *f.create(t, vpcA, failedVpc).VpcPeeringConnectionId

	active := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	f.activate(t, active)

	cases := []struct {
		name, id, from, to string
		op                 func(*testing.T, string) error
	}{
		{"accept rejected", rejected, StateRejected, StateProvisioning, func(t *testing.T, id string) error { _, err := f.accept(t, id); return err }},
		{"delete rejected", rejected, StateRejected, StateDeleting, f.del},
		{"accept failed", failed, StateFailed, StateProvisioning, func(t *testing.T, id string) error { _, err := f.accept(t, id); return err }},
		{"delete failed", failed, StateFailed, StateDeleting, f.del},
		{"reject failed", failed, StateFailed, StateRejected, f.reject},
		{"reject active", active, StateActive, StateRejected, f.reject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireMessage(t, tc.op(t, tc.id), awserrors.ErrorInvalidStateTransition,
				"Invalid state transition for "+tc.id+", attempted to transition from "+tc.from+" to "+tc.to)
		})
	}

	t.Run("unknown ID", func(t *testing.T) {
		const unknown = "pcx-0123456789abcdef0"
		requireMessage(t, f.del(t, unknown), awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound, "The vpcPeeringConnection ID '"+unknown+"' does not exist")
		_, err := f.describe(t, "pcx-bogus")
		requireMessage(t, err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound, "The vpcPeeringConnection ID 'pcx-bogus' does not exist")
	})
}

func TestOnePerPair(t *testing.T) {
	t.Run("reversed request cannot be accepted while the forward one is active", func(t *testing.T) {
		f := newFixture(t)
		fwd := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
		f.activate(t, fwd)
		rev := *f.create(t, vpcB, vpcA).VpcPeeringConnectionId
		_, err := f.accept(t, rev)
		requireMessage(t, err, awserrors.ErrorVpcPeeringConnectionAlreadyExists, "The vpc peering connection identified by "+rev+" already exists.")

		require.NoError(t, f.del(t, fwd))
		_, err = f.accept(t, rev)
		require.NoError(t, err, "after delete the reversed connection can be accepted")
	})

	for _, first := range []string{"forward", "reversed"} {
		t.Run("both pending, "+first+" accepted first", func(t *testing.T) {
			f := newFixture(t)
			ids := map[string]string{
				"forward":  *f.create(t, vpcA, vpcB).VpcPeeringConnectionId,
				"reversed": *f.create(t, vpcB, vpcA).VpcPeeringConnectionId,
			}
			second := map[string]string{"forward": "reversed", "reversed": "forward"}[first]
			_, err := f.accept(t, ids[first])
			require.NoError(t, err)
			_, err = f.accept(t, ids[second])
			requireCode(t, err, awserrors.ErrorVpcPeeringConnectionAlreadyExists)
		})
	}

	t.Run("concurrent accepts of both produce one provisioning connection", func(t *testing.T) {
		f := newFixture(t)
		ids := []string{*f.create(t, vpcA, vpcB).VpcPeeringConnectionId, *f.create(t, vpcB, vpcA).VpcPeeringConnectionId}
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Go(func() { _, _ = f.accept(t, id) })
		}
		wg.Wait()
		provisioning := 0
		for _, id := range ids {
			if f.status(t, id) == StateProvisioning {
				provisioning++
			}
		}
		assert.Equal(t, 1, provisioning)
	})
}

func TestAccept_CrashedPairClaimTakenOverOnlyAfterGrace(t *testing.T) {
	f := newFixture(t)
	fwd := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	rev := *f.create(t, vpcB, vpcA).VpcPeeringConnectionId
	// An Accept of fwd that crashed after its pair claim and before its record write.
	_, err := createJSON(t.Context(), f.svc.live, pairKey(vpcA, vpcB), claim{PcxID: fwd, ClaimedAt: f.clock.now()})
	require.NoError(t, err)

	_, err = f.accept(t, rev)
	requireCode(t, err, awserrors.ErrorVpcPeeringConnectionAlreadyExists)

	f.clock.advance(claimGrace)
	_, err = f.accept(t, rev)
	require.NoError(t, err, "a pending owner's claim older than the grace is taken over")
	_, err = f.accept(t, fwd)
	requireCode(t, err, awserrors.ErrorVpcPeeringConnectionAlreadyExists)
}

// TestAccept_TakeoverFencesTheOwnersRetry interleaves the owner's retried
// Accept between a takeover's read of the owner and its claim write: the
// owner's record is the fence, so exactly one connection provisions.
func TestAccept_TakeoverFencesTheOwnersRetry(t *testing.T) {
	f := newFixture(t)
	fwd := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	rev := *f.create(t, vpcB, vpcA).VpcPeeringConnectionId
	_, err := createJSON(t.Context(), f.svc.live, pairKey(vpcA, vpcB), claim{PcxID: fwd, ClaimedAt: f.clock.now()})
	require.NoError(t, err)
	f.clock.advance(claimGrace)

	// The takeover reads fwd's record; the owner's retried Accept then moves it.
	_, staleRev, err := getJSON[PeeringRecord](t.Context(), f.svc.live, recordKey(fwd))
	require.NoError(t, err)
	_, err = f.accept(t, fwd)
	require.NoError(t, err)
	stale := f.liveRecord(t, fwd)
	stale.Status = StatePendingAcceptance
	_, err = updateJSON(t.Context(), f.svc.live, recordKey(fwd), stale, staleRev)
	require.ErrorIs(t, err, errConflict, "the takeover's fence write loses to the owner's move")

	_, err = f.accept(t, rev)
	requireCode(t, err, awserrors.ErrorVpcPeeringConnectionAlreadyExists)
	assert.Equal(t, StateProvisioning, f.status(t, fwd))
	assert.Equal(t, StatePendingAcceptance, f.status(t, rev))
}

func TestCreate_InterruptedBeforeClaimIsAbsentThenSwept(t *testing.T) {
	f := newFixture(t)
	rec := &PeeringRecord{
		ID: "pcx-0000000000000dead", RequesterAccountID: testAccount, RequesterVpcID: vpcA, RequesterCIDR: "10.1.0.0/16",
		AccepterAccountID: testAccount, AccepterVpcID: vpcB, AccepterCIDR: "10.2.0.0/16", Region: testRegion,
		Status: StatePendingAcceptance, ExpirationTime: f.clock.now().Add(requestLifetime), Generation: 1, CreatedAt: f.clock.now(),
	}
	require.NoError(t, f.svc.writeIndexes(t.Context(), rec))
	_, err := createJSON(t.Context(), f.svc.live, recordKey(rec.ID), rec)
	require.NoError(t, err)

	assert.Empty(t, f.status(t, rec.ID), "Describe treats an unclaimed pending record as absent")
	_, err = f.accept(t, rec.ID)
	requireCode(t, err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound)
	requireCode(t, f.del(t, rec.ID), awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound)
	target, err := f.svc.RouteTarget(t.Context(), testAccount, rec.ID)
	require.NoError(t, err)
	assert.Nil(t, target)

	_, err = f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, f.liveKeys(t), "an in-flight create younger than the grace is untouched")

	f.clock.advance(claimGrace)
	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, res.DiscardedStale)
	assert.Empty(t, f.liveKeys(t))

	// A new request in that direction is not blocked.
	f.create(t, vpcA, vpcB)
}

func TestExpiry_ReadTimeThenSwept(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	f.clock.advance(requestLifetime)

	assert.Equal(t, StateExpired, f.status(t, id))
	_, err := f.accept(t, id)
	requireCode(t, err, awserrors.ErrorInvalidStateTransition)

	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Moved)
	assert.Equal(t, StateExpired, f.termRecord(t, id).Status)
	assert.Empty(t, f.liveKeys(t), "the sweep releases the claim and index keys")
	assert.Equal(t, StateExpired, f.status(t, id))

	res, err = f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, res.Moved, "the sweep is idempotent")
}

func TestExpiry_NewRequestKeepsTheExpiredOneVisible(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	f.clock.advance(requestLifetime)

	again := f.create(t, vpcA, vpcB)
	assert.NotEqual(t, id, *again.VpcPeeringConnectionId, "an expired request never blocks a new one")
	assert.Equal(t, StateExpired, f.status(t, id), "the expired request stays visible for its window")
	assert.Equal(t, StatePendingAcceptance, f.status(t, *again.VpcPeeringConnectionId))
}

func TestSweep_FinishesAMoveInterruptedAfterTheFence(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	rec, rev, err := getJSON[PeeringRecord](t.Context(), f.svc.live, recordKey(id))
	require.NoError(t, err)
	rec.Status, rec.StatusMessage, rec.TerminalAt, rec.TerminatedBy = StateRejected, rejectedMessage(testAccount), f.clock.now(), testAccount
	_, err = updateJSON(t.Context(), f.svc.live, recordKey(id), rec, rev)
	require.NoError(t, err)

	assert.Equal(t, StateRejected, f.status(t, id))
	_, err = f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Empty(t, f.liveKeys(t))
	assert.Equal(t, StateRejected, f.termRecord(t, id).Status)
}

func TestSweep_RemovesOrphanedKeysAfterGrace(t *testing.T) {
	f := newFixture(t)
	const ghost = "pcx-00000000000000bad"
	entry := indexEntry{CreatedAt: f.clock.now()}
	require.NoError(t, putJSON(t.Context(), f.svc.live, acctKey(testAccount, ghost), entry))
	require.NoError(t, putJSON(t.Context(), f.svc.live, vpcKey(vpcA, ghost), entry))
	require.NoError(t, putJSON(t.Context(), f.svc.live, reqKey(vpcA, vpcC), claim{PcxID: ghost, ClaimedAt: f.clock.now()}))
	keep := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId

	_, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.True(t, f.has(t, vpcKey(vpcA, ghost)), "residue younger than the grace stays")

	f.clock.advance(claimGrace)
	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, res.KeysRemoved)
	assert.False(t, f.has(t, vpcKey(vpcA, ghost)))
	assert.True(t, f.has(t, vpcKey(vpcA, keep)), "a valid connection's keys stay")
	assert.Equal(t, StatePendingAcceptance, f.status(t, keep))
}

func TestPendingWithDeletedVpc_EndsRejectedWhicheverRunsFirst(t *testing.T) {
	for _, end := range []string{vpcA, vpcB} {
		f := newFixture(t)
		id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
		f.deleteVPC(t, testAccount, end)

		conns, err := f.describe(t, id)
		require.NoError(t, err)
		assert.Equal(t, StateRejected, *conns[0].Status.Code)
		assert.Equal(t, "Rejected by "+testAccount, *conns[0].Status.Message)

		require.NoError(t, f.svc.MarkCleaned(t.Context(), id), "the reconciler never marks a pending connection")
		assert.Equal(t, StatePendingAcceptance, f.liveRecord(t, id).Status)

		_, err = f.svc.Sweep(t.Context())
		require.NoError(t, err)
		assert.Equal(t, StateRejected, f.termRecord(t, id).Status)
		require.NoError(t, f.svc.MarkCleaned(t.Context(), id))
		assert.Equal(t, StateRejected, f.status(t, id))
	}
}

func TestLinkedWithDeletedVpc_CleanedAsDeleted(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	f.activate(t, id)
	f.deleteVPC(t, testAccount, vpcB)

	require.NoError(t, f.svc.MarkCleaned(t.Context(), id))
	assert.Equal(t, StateDeleted, f.termRecord(t, id).Status)
	assert.Empty(t, f.liveKeys(t))
}

func TestModifyOptions(t *testing.T) {
	f := newFixture(t)
	f.putVPC(t, testAccount, vpcID(30), "10.240.0.0/16")
	f.putVPC(t, testAccount, vpcID(31), "172.31.0.0/16")
	id := *f.create(t, vpcID(30), vpcID(31)).VpcPeeringConnectionId
	modify := func(req, acc *ec2.PeeringConnectionOptionsRequest) (*ec2.ModifyVpcPeeringConnectionOptionsOutput, error) {
		return f.svc.ModifyVpcPeeringConnectionOptions(t.Context(), &ec2.ModifyVpcPeeringConnectionOptionsInput{
			VpcPeeringConnectionId: aws.String(id), RequesterPeeringConnectionOptions: req, AccepterPeeringConnectionOptions: acc,
		}, testAccount)
	}
	dns := &ec2.PeeringConnectionOptionsRequest{AllowDnsResolutionFromRemoteVpc: aws.Bool(true)}

	_, err := modify(dns, nil)
	requireMessage(t, err, awserrors.ErrorOperationNotPermitted, "Peering "+id+" is not active. Peering options can be added only to active peerings.")

	f.activate(t, id)
	_, err = modify(dns, nil)
	requireMessage(t, err, awserrors.ErrorInvalidVpcPeeringConnectionStateDnsHostnamesDisabled, "Public Hostnames are disabled for: "+vpcID(30))

	_, err = modify(&ec2.PeeringConnectionOptionsRequest{AllowEgressFromLocalVpcToRemoteClassicLink: aws.Bool(true)}, nil)
	requireMessage(t, err, awserrors.ErrorCidrConflict, "The CIDR range of "+vpcID(30)+" overlaps with the Classic IP space")

	_, err = modify(nil, &ec2.PeeringConnectionOptionsRequest{AllowEgressFromLocalVpcToRemoteClassicLink: aws.Bool(true)})
	require.NoError(t, err, "172.31.0.0/16 is outside the Classic IP space")

	f.updateVPC(t, testAccount, vpcID(30), func(r *ec2vpc.VPCRecord) { r.EnableDnsHostnames = true })
	out, err := modify(dns, &ec2.PeeringConnectionOptionsRequest{AllowEgressFromLocalClassicLinkToRemoteVpc: aws.Bool(true)})
	require.NoError(t, err)
	assert.True(t, *out.RequesterPeeringConnectionOptions.AllowDnsResolutionFromRemoteVpc)
	assert.True(t, *out.AccepterPeeringConnectionOptions.AllowEgressFromLocalClassicLinkToRemoteVpc)
	assert.True(t, *out.AccepterPeeringConnectionOptions.AllowEgressFromLocalVpcToRemoteClassicLink, "earlier options are kept")

	conns, err := f.describe(t, id)
	require.NoError(t, err)
	assert.True(t, *conns[0].RequesterVpcInfo.PeeringOptions.AllowDnsResolutionFromRemoteVpc)

	_, err = f.svc.ModifyVpcPeeringConnectionOptions(t.Context(), &ec2.ModifyVpcPeeringConnectionOptionsInput{VpcPeeringConnectionId: aws.String("pcx-0123456789abcdef0")}, testAccount)
	requireMessage(t, err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound, "The peering ID 'pcx-0123456789abcdef0' does not exist")
}

func TestClassicIPSpace(t *testing.T) {
	for cidr, want := range map[string]bool{
		"10.0.0.0/16": false, "10.1.0.0/16": false, "10.1.4.0/24": false,
		"10.2.0.0/16": true, "10.240.0.0/16": true, "172.16.0.0/16": false, "192.168.0.0/16": false,
	} {
		assert.Equal(t, want, overlapsClassicIPSpace(cidr), cidr)
	}
}

func TestDescribe_FiltersAndShapes(t *testing.T) {
	f := newFixture(t)
	pending := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	active := *f.create(t, vpcA, vpcC).VpcPeeringConnectionId
	f.activate(t, active)
	deleted := *f.create(t, vpcB, vpcC).VpcPeeringConnectionId
	require.NoError(t, f.del(t, deleted))

	describe := func(filters ...*ec2.Filter) []string {
		out, err := f.svc.DescribeVpcPeeringConnections(t.Context(), &ec2.DescribeVpcPeeringConnectionsInput{Filters: filters}, testAccount)
		require.NoError(t, err)
		var ids []string
		for _, c := range out.VpcPeeringConnections {
			ids = append(ids, *c.VpcPeeringConnectionId)
		}
		return ids
	}
	filter := func(name string, values ...string) *ec2.Filter {
		return &ec2.Filter{Name: aws.String(name), Values: aws.StringSlice(values)}
	}

	assert.ElementsMatch(t, []string{pending, active, deleted}, describe())
	assert.Equal(t, []string{active}, describe(filter("status-code", StateActive)))
	assert.Equal(t, []string{deleted}, describe(filter("status-message", "Deleted by *")))
	assert.ElementsMatch(t, []string{pending, active}, describe(filter("requester-vpc-info.vpc-id", vpcA)))
	assert.Equal(t, []string{active}, describe(filter("accepter-vpc-info.cidr-block", "10.3.0.0/16")), "a deleted connection has no CIDR to match")
	assert.Equal(t, []string{pending}, describe(filter("expiration-time", awsTime(f.clock.now().Add(requestLifetime)))))

	conns, err := f.describe(t, deleted)
	require.NoError(t, err)
	assert.Nil(t, conns[0].RequesterVpcInfo.CidrBlock, "a terminal connection carries only identities")
	assert.Nil(t, conns[0].ExpirationTime)

	_, err = f.svc.DescribeVpcPeeringConnections(t.Context(), &ec2.DescribeVpcPeeringConnectionsInput{Filters: []*ec2.Filter{filter("vpc-id", vpcA)}}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestTags_PerAccountAndFiltered(t *testing.T) {
	f := newFixture(t)
	out, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{
		VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcB),
		TagSpecifications: []*ec2.TagSpecification{{ResourceType: aws.String(ec2.ResourceTypeVpcPeeringConnection), Tags: []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("ab")}}}},
	}, testAccount)
	require.NoError(t, err)
	id := *out.VpcPeeringConnection.VpcPeeringConnectionId
	require.Len(t, out.VpcPeeringConnection.Tags, 1)

	require.NoError(t, f.svc.ApplyRecordTags(&ec2.CreateTagsInput{Resources: aws.StringSlice([]string{id}), Tags: []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("dev")}}}, testAccount))
	require.NoError(t, f.svc.ApplyRecordTags(&ec2.CreateTagsInput{Resources: aws.StringSlice([]string{id}), Tags: []*ec2.Tag{{Key: aws.String("x"), Value: aws.String("y")}}}, "210987654321"))
	assert.Equal(t, map[string]map[string]string{testAccount: {"Name": "ab", "env": "dev"}}, f.liveRecord(t, id).Tags, "an account that does not own the connection writes nothing")

	got, err := f.svc.DescribeVpcPeeringConnections(t.Context(), &ec2.DescribeVpcPeeringConnectionsInput{Filters: []*ec2.Filter{{Name: aws.String("tag:env"), Values: aws.StringSlice([]string{"dev"})}}}, testAccount)
	require.NoError(t, err)
	require.Len(t, got.VpcPeeringConnections, 1)

	require.NoError(t, f.svc.RemoveRecordTags(&ec2.DeleteTagsInput{Resources: aws.StringSlice([]string{id})}, testAccount))
	conns, err := f.describe(t, id)
	require.NoError(t, err)
	assert.NotNil(t, conns[0].Tags)
	assert.Empty(t, conns[0].Tags)
}

func TestLimits(t *testing.T) {
	f := newFixture(t)
	for i := range outstandingLimit {
		peer := vpcID(100 + i)
		f.putVPC(t, testAccount, peer, "10.100."+strconv.Itoa(i)+".0/24")
		f.create(t, vpcA, peer)
	}
	peer := vpcID(200)
	f.putVPC(t, testAccount, peer, "10.200.0.0/24")
	_, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpcA), PeerVpcId: aws.String(peer)}, testAccount)
	requireCode(t, err, awserrors.ErrorOutstandingVpcPeeringConnectionLimitExceeded)
}

func TestVpcHasLinkedConnection(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	has, err := f.svc.VpcHasLinkedConnection(t.Context(), vpcA)
	require.NoError(t, err)
	assert.False(t, has, "a pending connection does not block DeleteVpc")

	f.activate(t, id)
	has, err = f.svc.VpcHasLinkedConnection(t.Context(), vpcB)
	require.NoError(t, err)
	assert.True(t, has)

	require.NoError(t, f.del(t, id))
	has, err = f.svc.VpcHasLinkedConnection(t.Context(), vpcA)
	require.NoError(t, err)
	assert.False(t, has, "a deleted connection awaiting cleanup does not block DeleteVpc")
}

func TestRouteTargetAndActiveConnections(t *testing.T) {
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId

	target, err := f.svc.RouteTarget(t.Context(), testAccount, id)
	require.NoError(t, err)
	require.NotNil(t, target, "a pending connection is a route target")
	assert.Equal(t, vpcA, target.RequesterVpcID)

	states, err := f.svc.ActiveConnections(t.Context(), []string{id})
	require.NoError(t, err)
	assert.False(t, states[id])

	f.activate(t, id)
	states, err = f.svc.ActiveConnections(t.Context(), []string{id})
	require.NoError(t, err)
	assert.True(t, states[id])

	require.NoError(t, f.del(t, id))
	target, err = f.svc.RouteTarget(t.Context(), testAccount, id)
	require.NoError(t, err)
	assert.Nil(t, target, "a deleted connection is not a route target")
}

func countPrefix(keys map[string]bool, prefix string) int {
	n := 0
	for k := range keys {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// The GC ticks every few minutes; the sweep reaper scans at most hourly, and
// the first tick after a leader change scans at once.
func TestSweepReaper_RunsHourly(t *testing.T) {
	f := newFixture(t)
	r := f.svc.NewSweepReaper()
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	f.clock.advance(requestLifetime)

	n, err := r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the first tick sweeps the expired request")
	assert.Equal(t, StateExpired, f.termRecord(t, id).Status)

	id = *f.create(t, vpcA, vpcC).VpcPeeringConnectionId
	f.deleteVPC(t, testAccount, vpcC)
	f.clock.advance(10 * time.Minute)
	n, err = r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n, "a tick within the hour does not scan")
	assert.NotNil(t, f.liveRecord(t, id))

	f.clock.advance(SweepInterval)
	n, err = r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}
