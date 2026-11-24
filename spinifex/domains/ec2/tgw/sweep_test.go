package tgw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An orphan index key or claim, left by a crash before the record write, is
// kept through the grace and removed after it.
func TestSweep_OrphanKeysWaitForTheGrace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	orphan := &AttachmentRecord{ID: "tgw-attach-0123456789abcdef0", AccountID: testAccount, TransitGatewayID: "tgw-0123456789abcdef0", VpcID: "vpc-00000001", SubnetIDs: []string{"subnet-00000001"}, CreatedAt: f.clock.Now()}
	require.NoError(t, f.svc.writeAttachmentIndexes(ctx, orphan))
	_, err := f.svc.takeClaim(ctx, f.svc.atts, pairKey(orphan.TransitGatewayID, orphan.VpcID), f.svc.attachmentClaim(orphan), f.svc.attachmentHolderStale)
	require.NoError(t, err)
	written := len(keys(t, f.svc.atts))

	blocked, err := f.svc.VpcBlocksDeletion(ctx, testAccount, orphan.VpcID)
	require.NoError(t, err)
	assert.True(t, blocked, "a young orphan key blocks the parent delete")
	res, err := f.svc.Sweep(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.KeysRemoved)
	assert.Len(t, keys(t, f.svc.atts), written)

	f.clock.Advance(claimGrace)
	blocked, err = f.svc.VpcBlocksDeletion(ctx, testAccount, orphan.VpcID)
	require.NoError(t, err)
	assert.False(t, blocked)
	res, err = f.svc.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, written, res.KeysRemoved)
	assert.Empty(t, keys(t, f.svc.atts))
}

// The sweep never removes the record, keys or claims of a live attachment,
// however old they are.
func TestSweep_KeepsEverythingALiveAttachmentHolds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	att, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	_, err = f.svc.PrepareAttachment(t.Context(), testAccount, aws.StringValue(att.TransitGatewayAttachmentId))
	require.NoError(t, err)
	attKeys, tgwKeys := keys(t, f.svc.atts), keys(t, f.svc.tgws)

	f.clock.Advance(10 * transitGatewayWindow)
	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SweepResult{}, res)
	assert.ElementsMatch(t, attKeys, keys(t, f.svc.atts))
	assert.ElementsMatch(t, tgwKeys, keys(t, f.svc.tgws))
}

// A deleted record stays visible through its window, and the sweep removes it
// only after the window.
func TestSweep_TerminalRecordsExpireAfterTheWindow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.availableTGW()
	_, err := f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(id)}, testAccount)
	require.NoError(t, err)
	require.NoError(t, f.svc.MarkTransitGatewayCleaned(t.Context(), testAccount, id))

	describe := func() []*ec2.TransitGateway {
		out, err := f.svc.DescribeTransitGateways(t.Context(), &ec2.DescribeTransitGatewaysInput{}, testAccount)
		require.NoError(t, err)
		return out.TransitGateways
	}
	require.Len(t, describe(), 1)
	assert.Equal(t, StateDeleted, aws.StringValue(describe()[0].State))

	f.clock.Advance(transitGatewayWindow - 1)
	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, res.Expired)
	assert.Len(t, describe(), 1)

	f.clock.Advance(1)
	assert.Empty(t, describe(), "a record past its window is not described")
	res, err = f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Expired)
	assert.Empty(t, keys(t, f.svc.term))
}

// A move interrupted after its terminal state was written in the live bucket
// is finished by the sweep.
func TestSweep_FinishesAnInterruptedMove(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.availableTGW()
	rec, rev, err := f.svc.transitGateway(t.Context(), testAccount, id)
	require.NoError(t, err)
	rec.State, rec.TerminalAt = StateDeleted, f.clock.Now()
	_, err = writeJSON(t.Context(), f.svc.tgws, recordKey(testAccount, id), rec, rev)
	require.NoError(t, err)

	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Moved)
	assert.Empty(t, keys(t, f.svc.tgws), "record, account key and slot are gone")
	term, err := f.svc.terminalTransitGateway(t.Context(), testAccount, id)
	require.NoError(t, err)
	require.NotNil(t, term)
}

func TestSweepReaper_RunsAtMostOncePerInterval(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.svc.NewSweepReaper()
	orphan := &AttachmentRecord{ID: "tgw-attach-0123456789abcdef0", AccountID: testAccount, TransitGatewayID: "tgw-0123456789abcdef0", VpcID: "vpc-00000001", CreatedAt: f.clock.Now()}
	f.clock.Advance(claimGrace)
	_, err := r.Sweep(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.svc.writeAttachmentIndexes(t.Context(), orphan))
	f.clock.Advance(claimGrace)
	n, err := r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n, "inside the interval the sweep does not run")
	f.clock.Advance(SweepInterval)
	n, err = r.Sweep(t.Context())
	require.NoError(t, err)
	assert.Positive(t, n)
}

// With a pool of two links, a third attachment moves to failed and releases
// its claims; deleting one attachment frees a link for the next.
func TestPrepareAttachment_FullLinkPoolFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.svc.slots = 2
	tgwID := f.availableTGW()
	prepare := func(vpc string) string {
		vpcID, subnetID := f.network(vpc)
		att, err := f.attach(tgwID, vpcID, subnetID)
		require.NoError(t, err)
		id := aws.StringValue(att.TransitGatewayAttachmentId)
		_, err = f.svc.PrepareAttachment(t.Context(), testAccount, id)
		require.NoError(t, err)
		return id
	}
	first, second := prepare("vpc-00000001"), prepare("vpc-00000002")
	a, _, err := f.svc.attachment(t.Context(), testAccount, first)
	require.NoError(t, err)
	b, _, err := f.svc.attachment(t.Context(), testAccount, second)
	require.NoError(t, err)
	assert.NotEqual(t, a.LinkPrefix, b.LinkPrefix, "two attachments get different links")

	third := prepare("vpc-00000003")
	rec, _, err := f.svc.attachment(t.Context(), testAccount, third)
	require.NoError(t, err)
	assert.Nil(t, rec, "the failed attachment left the live bucket")
	term, err := f.svc.terminalAttachment(t.Context(), testAccount, third)
	require.NoError(t, err)
	require.NotNil(t, term)
	assert.Equal(t, StateFailed, term.State)
	blocked, err := f.svc.VpcBlocksDeletion(t.Context(), testAccount, "vpc-00000003")
	require.NoError(t, err)
	assert.False(t, blocked, "a failed attachment releases its claims and keys")
	routable, err := f.svc.RouteTarget(t.Context(), testAccount, tgwID, "vpc-00000003")
	require.NoError(t, err)
	assert.False(t, routable)
}

// A parent removed after admission makes the reconciler fail the attachment.
func TestPrepareAttachment_AbsentParentFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	att, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	f.markVPC(vpcID)
	rec, err := f.svc.PrepareAttachment(t.Context(), testAccount, aws.StringValue(att.TransitGatewayAttachmentId))
	require.NoError(t, err)
	assert.Nil(t, rec)
	term, err := f.svc.terminalAttachment(t.Context(), testAccount, aws.StringValue(att.TransitGatewayAttachmentId))
	require.NoError(t, err)
	require.NotNil(t, term)
	assert.Equal(t, StateFailed, term.State)
}

// A late confirmation for an earlier generation changes nothing.
func TestMarkAvailable_IgnoresAStaleGeneration(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	require.NoError(t, err)
	id := aws.StringValue(out.TransitGateway.TransitGatewayId)
	_, err = f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(id)}, testAccount)
	require.NoError(t, err)
	require.NoError(t, f.svc.MarkTransitGatewayAvailable(t.Context(), testAccount, id, 1))
	rec, _, err := f.svc.transitGateway(t.Context(), testAccount, id)
	require.NoError(t, err)
	assert.Equal(t, StateDeleting, rec.State)
}

func TestAccountResources_ListsTheAccountsRecords(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	_, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	atts, tgws, err := f.svc.AccountResources(t.Context(), testAccount)
	require.NoError(t, err)
	assert.Len(t, atts, 1)
	assert.Len(t, tgws, 1)
	atts, tgws, err = f.svc.AccountResources(t.Context(), "210987654321")
	require.NoError(t, err)
	assert.Empty(t, atts)
	assert.Empty(t, tgws)
}
