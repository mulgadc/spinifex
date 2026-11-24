package tgw

import (
	"fmt"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateTransitGateway_DefaultsAndDerivedRouteTable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{Description: aws.String("hub")}, testAccount)
	require.NoError(t, err)
	tgw := out.TransitGateway
	id := aws.StringValue(tgw.TransitGatewayId)
	assert.True(t, wellFormed(id, "tgw-"), id)
	assert.Equal(t, StatePending, aws.StringValue(tgw.State))
	assert.Equal(t, int64(64512), aws.Int64Value(tgw.Options.AmazonSideAsn))
	rtb := "tgw-rtb-" + id[len("tgw-"):]
	assert.Equal(t, rtb, aws.StringValue(tgw.Options.AssociationDefaultRouteTableId))
	assert.Equal(t, rtb, aws.StringValue(tgw.Options.PropagationDefaultRouteTableId))
	assert.Equal(t, "arn:aws:ec2:ap-southeast-2:"+testAccount+":transit-gateway/"+id, aws.StringValue(tgw.TransitGatewayArn))
	assert.NotNil(t, tgw.Tags, "Create renders an empty tag list")
	assert.Zero(t, f.gate.held(), "admission is released")
}

func TestCreateTransitGateway_RefusesOptionsItDoesNotOffer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for name, opts := range map[string]*ec2.TransitGatewayRequestOptions{
		"association off": {DefaultRouteTableAssociation: aws.String(disable)},
		"propagation off": {DefaultRouteTablePropagation: aws.String(disable)},
		"multicast":       {MulticastSupport: aws.String(enable)},
		"cidr blocks":     {TransitGatewayCidrBlocks: aws.StringSlice([]string{"10.99.0.0/24"})},
		"private asn low": {AmazonSideAsn: aws.Int64(64511)},
		"bad enable word": {DnsSupport: aws.String("yes")},
	} {
		_, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{Options: opts}, testAccount)
		code, _ := awserrors.ResolveErrorCode(err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, code, name)
	}
	assert.Empty(t, keys(t, f.svc.tgws), "a refused create writes nothing")
}

func TestTransitGateways_NotOfferedBelowTheLevel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.gate.ok = false
	_, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	requireCode(t, err, awserrors.ErrorUnsupportedOperation)
	_, err = f.svc.DescribeTransitGateways(t.Context(), &ec2.DescribeTransitGatewaysInput{}, testAccount)
	requireCode(t, err, awserrors.ErrorUnsupportedOperation)
	_, err = f.attach("tgw-0123456789abcdef0", "vpc-00000001", "subnet-00000001")
	requireCode(t, err, awserrors.ErrorUnsupportedOperation)
	assert.Empty(t, keys(t, f.svc.tgws))
	assert.Empty(t, keys(t, f.svc.atts))
	assert.Zero(t, f.gate.held(), "every path releases the admission")

	f.gate.ok, f.gate.err = true, fmt.Errorf("level store unreachable")
	_, err = f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	requireCode(t, err, awserrors.ErrorServerInternal)
	assert.Zero(t, f.gate.held())
}

func TestCreateTransitGateway_QuotaOfFive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var ids []string
	for range accountLimit {
		out, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
		require.NoError(t, err)
		ids = append(ids, aws.StringValue(out.TransitGateway.TransitGatewayId))
	}
	_, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceLimitExceeded)

	// A deleting gateway still counts; a deleted one frees its slot.
	_, err = f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(ids[0])}, testAccount)
	require.NoError(t, err)
	_, err = f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceLimitExceeded)
	require.NoError(t, f.svc.MarkTransitGatewayCleaned(t.Context(), testAccount, ids[0]))
	_, err = f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	require.NoError(t, err)
}

func TestCreateTransitGateway_ConcurrentCreatesNeverPassTheQuota(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
		})
	}
	wg.Wait()
	created := 0
	for _, err := range errs {
		if err == nil {
			created++
			continue
		}
		requireCode(t, err, awserrors.ErrorResourceLimitExceeded)
	}
	assert.Equal(t, accountLimit, created)
	recs, err := f.svc.ListTransitGateways(t.Context())
	require.NoError(t, err)
	assert.Len(t, recs, accountLimit)
}

// A slot whose holder never wrote its record is taken over only after the grace.
func TestCreateTransitGateway_StaleSlotReclaimedAfterGrace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for n := range accountLimit {
		require.NoError(t, putJSON(t.Context(), f.svc.tgws, slotKey(testAccount, n),
			claim{Owner: fmt.Sprintf("tgw-%017x", n), AccountID: testAccount, ClaimedAt: f.clock.Now()}))
	}
	_, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceLimitExceeded)
	f.clock.Advance(claimGrace)
	_, err = f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	require.NoError(t, err)
}

func TestCreateTransitGateway_InactiveAccountLeavesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.accounts.active = false
	_, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	requireCode(t, err, awserrors.ErrorAuthFailure)
	assert.Empty(t, keys(t, f.svc.tgws), "record, account key and slot are removed")

	f.accounts.active, f.accounts.err = false, fmt.Errorf("accounts bucket unreachable")
	_, err = f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	requireCode(t, err, awserrors.ErrorServerInternal)
	assert.Empty(t, keys(t, f.svc.tgws))

	f.svc.SetAccountStatus(nil)
	_, err = f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	require.Error(t, err, "with no status reader every create is refused")
	assert.Empty(t, keys(t, f.svc.tgws))
}

func TestDeleteTransitGateway_IDsAndStates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String("tgw-bogus")}, testAccount)
	assert.Equal(t, "Invalid Transit Gateway id tgw-bogus.", requireCode(t, err, awserrors.ErrorInvalidTransitGatewayIDMalformed))
	_, err = f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String("tgw-0123456789abcdef0")}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidTransitGatewayIDNotFound)

	id := f.availableTGW()
	out, err := f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(id)}, testAccount)
	require.NoError(t, err)
	assert.Equal(t, StateDeleting, aws.StringValue(out.TransitGateway.State))
	assert.Nil(t, out.TransitGateway.Tags, "a delete response omits tags")
	again, err := f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(id)}, testAccount)
	require.NoError(t, err)
	assert.Equal(t, StateDeleting, aws.StringValue(again.TransitGateway.State))

	require.NoError(t, f.svc.MarkTransitGatewayCleaned(t.Context(), testAccount, id))
	gone, err := f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(id)}, testAccount)
	require.NoError(t, err, "a delete of a deleted gateway succeeds")
	assert.Equal(t, StateDeleted, aws.StringValue(gone.TransitGateway.State))
}

func TestDeleteTransitGateway_RefusedWhileAttached(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	att, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	_, err = f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(tgwID)}, testAccount)
	msg := requireCode(t, err, awserrors.ErrorIncorrectState)
	assert.Equal(t, tgwID+" has non-deleted VPC Attachments: "+aws.StringValue(att.TransitGatewayAttachmentId)+".", msg)
}

func TestCreateAttachment_ParentChecks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")

	_, err := f.attach("tgw-bogus", vpcID, subnetID)
	requireCode(t, err, awserrors.ErrorInvalidTransitGatewayIDMalformed)
	_, err = f.attach("tgw-0123456789abcdef0", vpcID, subnetID)
	requireCode(t, err, awserrors.ErrorInvalidTransitGatewayIDNotFound)
	_, err = f.attach(tgwID, vpcID)
	requireCode(t, err, awserrors.ErrorMissingParameter)
	_, err = f.attach(tgwID, "vpc-0000000f", subnetID)
	requireCode(t, err, awserrors.ErrorInvalidVpcIDNotFound)
	_, err = f.attach(tgwID, vpcID, "subnet-0000000f")
	requireCode(t, err, awserrors.ErrorInvalidSubnetIDNotFound)

	f.putSubnet("subnet-00000002", vpcID, "ap-southeast-2a")
	_, err = f.attach(tgwID, vpcID, subnetID, "subnet-00000002")
	requireCode(t, err, awserrors.ErrorDuplicateSubnetsInSameZone)

	f.putVPC("vpc-00000003", "169.254.10.0/24")
	f.putSubnet("subnet-00000003", "vpc-00000003", "ap-southeast-2a")
	_, err = f.attach(tgwID, "vpc-00000003", "subnet-00000003")
	requireCode(t, err, awserrors.ErrorInvalidParameterValue)

	pending, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	require.NoError(t, err)
	_, err = f.attach(aws.StringValue(pending.TransitGateway.TransitGatewayId), vpcID, subnetID)
	requireCode(t, err, awserrors.ErrorIncorrectState)

	assert.Empty(t, keys(t, f.svc.atts), "every refusal leaves no record, key or claim")
}

func TestCreateAttachment_MarkedParentIsAbsent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	f.markVPC(vpcID)
	_, err := f.attach(tgwID, vpcID, subnetID)
	requireCode(t, err, awserrors.ErrorInvalidVpcIDNotFound)
	assert.Empty(t, keys(t, f.svc.atts))
}

func TestCreateAttachment_OneLiveAttachmentPerVpc(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { _, errs[i] = f.attach(tgwID, vpcID, subnetID) })
	}
	wg.Wait()
	created := 0
	for _, err := range errs {
		if err == nil {
			created++
			continue
		}
		code, _ := awserrors.ResolveErrorCode(err)
		assert.Contains(t, []string{awserrors.ErrorDuplicateTransitGatewayAttachment, awserrors.ErrorServerInternal}, code)
	}
	assert.Equal(t, 1, created)
	atts, err := f.svc.ListAttachments(t.Context())
	require.NoError(t, err)
	assert.Len(t, atts, 1)
}

func TestCreateAttachment_InactiveAccountLeavesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	f.accounts.active = false
	_, err := f.attach(tgwID, vpcID, subnetID)
	requireCode(t, err, awserrors.ErrorAuthFailure)
	assert.Empty(t, keys(t, f.svc.atts))
	blocked, err := f.svc.VpcBlocksDeletion(t.Context(), testAccount, vpcID)
	require.NoError(t, err)
	assert.False(t, blocked)
}

func TestAttachment_LifecycleBlocksParentsUntilDeleted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	att, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	attID := aws.StringValue(att.TransitGatewayAttachmentId)
	assert.Equal(t, StatePending, aws.StringValue(att.State))
	assert.Equal(t, []string{subnetID}, aws.StringValueSlice(att.SubnetIds))

	for name, check := range map[string]func() (bool, error){
		"vpc":    func() (bool, error) { return f.svc.VpcBlocksDeletion(t.Context(), testAccount, vpcID) },
		"subnet": func() (bool, error) { return f.svc.SubnetBlocksDeletion(t.Context(), testAccount, subnetID) },
	} {
		blocked, err := check()
		require.NoError(t, err)
		assert.True(t, blocked, name)
	}
	routable, err := f.svc.RouteTarget(t.Context(), testAccount, tgwID, vpcID)
	require.NoError(t, err)
	assert.True(t, routable, "a pending attachment admits a route")
	active, err := f.svc.RouteActive(t.Context(), testAccount, tgwID, vpcID)
	require.NoError(t, err)
	assert.False(t, active, "a pending attachment does not forward")

	rec, err := f.svc.PrepareAttachment(t.Context(), testAccount, attID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.NotEmpty(t, rec.LinkPrefix)
	require.NoError(t, f.svc.MarkAttachmentAvailable(t.Context(), testAccount, attID, rec.Generation))
	active, err = f.svc.RouteActive(t.Context(), testAccount, tgwID, vpcID)
	require.NoError(t, err)
	assert.True(t, active)

	del, err := f.svc.DeleteTransitGatewayVpcAttachment(t.Context(), &ec2.DeleteTransitGatewayVpcAttachmentInput{TransitGatewayAttachmentId: aws.String(attID)}, testAccount)
	require.NoError(t, err)
	assert.Equal(t, StateDeleting, aws.StringValue(del.TransitGatewayVpcAttachment.State))
	assert.Nil(t, del.TransitGatewayVpcAttachment.SubnetIds, "a delete response omits subnets")
	blocked, err := f.svc.VpcBlocksDeletion(t.Context(), testAccount, vpcID)
	require.NoError(t, err)
	assert.True(t, blocked, "a deleting attachment still blocks")

	require.NoError(t, f.svc.MarkAttachmentCleaned(t.Context(), testAccount, attID))
	blocked, err = f.svc.VpcBlocksDeletion(t.Context(), testAccount, vpcID)
	require.NoError(t, err)
	assert.False(t, blocked)
	assert.Empty(t, keys(t, f.svc.atts), "the move releases the claims and keys")

	out, err := f.svc.DescribeTransitGatewayVpcAttachments(t.Context(), &ec2.DescribeTransitGatewayVpcAttachmentsInput{
		TransitGatewayAttachmentIds: aws.StringSlice([]string{attID}),
	}, testAccount)
	require.NoError(t, err)
	require.Len(t, out.TransitGatewayVpcAttachments, 1)
	assert.Equal(t, StateDeleted, aws.StringValue(out.TransitGatewayVpcAttachments[0].State))
	assert.Empty(t, out.TransitGatewayVpcAttachments[0].SubnetIds)

	again, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err, "the pair is free again")
	assert.NotEqual(t, attID, aws.StringValue(again.TransitGatewayAttachmentId))
}

func TestDeleteAttachment_IDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.svc.DeleteTransitGatewayVpcAttachment(t.Context(), &ec2.DeleteTransitGatewayVpcAttachmentInput{TransitGatewayAttachmentId: aws.String("tgw-attach-bogus")}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidTransitGatewayAttachmentIDMalformed)
	_, err = f.svc.DeleteTransitGatewayVpcAttachment(t.Context(), &ec2.DeleteTransitGatewayVpcAttachmentInput{TransitGatewayAttachmentId: aws.String("tgw-attach-0123456789abcdef0")}, testAccount)
	assert.Equal(t, "Transit Gateway VPC Attachment tgw-attach-0123456789abcdef0 was deleted or does not exist.",
		requireCode(t, err, awserrors.ErrorInvalidTransitGatewayAttachmentIDNotFound))
}

// A parent rewritten after admission read it is re-read: a marked parent
// makes admission undo everything it wrote.
func TestFence_MarkAfterCheckUndoesAdmission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	revs, err := f.svc.checkParents(t.Context(), testAccount, tgwID, vpcID, []string{subnetID})
	require.NoError(t, err)
	f.markVPC(vpcID)
	rec := &AttachmentRecord{ID: "tgw-attach-0123456789abcdef0", AccountID: testAccount, TransitGatewayID: tgwID, VpcID: vpcID, SubnetIDs: []string{subnetID}}
	err = f.svc.fenceParents(t.Context(), rec, revs)
	requireCode(t, err, awserrors.ErrorInvalidVpcIDNotFound)

	// An unmarked rewrite only moves the revision; the fence re-reads and passes.
	f.putVPC(vpcID, "10.0.0.0/16")
	revs, err = f.svc.checkParents(t.Context(), testAccount, tgwID, vpcID, []string{subnetID})
	require.NoError(t, err)
	f.putVPC(vpcID, "10.0.0.0/16")
	require.NoError(t, f.svc.fenceParents(t.Context(), rec, revs))
}

// Admission's rewrite moves the parent's revision, so a delete that checked
// the revision before it loses its marked write.
func TestFence_AdmissionRewriteBeatsAStaleMark(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	_, before, err := f.svc.parents.VPC(t.Context(), testAccount, vpcID)
	require.NoError(t, err)
	_, err = f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	_, after, err := f.svc.parents.VPC(t.Context(), testAccount, vpcID)
	require.NoError(t, err)
	assert.Greater(t, after, before, "admission rewrote the VPC")
	assert.ErrorIs(t, f.svc.parents.TouchVPC(t.Context(), testAccount, vpcID, before), ec2vpc.ErrParentChanged)
}

func TestDescribeTransitGateways_FiltersAndIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.availableTGW()
	_, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	require.NoError(t, err)

	out, err := f.svc.DescribeTransitGateways(t.Context(), &ec2.DescribeTransitGatewaysInput{}, testAccount)
	require.NoError(t, err)
	assert.Len(t, out.TransitGateways, 2)
	out, err = f.svc.DescribeTransitGateways(t.Context(), &ec2.DescribeTransitGatewaysInput{
		Filters: []*ec2.Filter{{Name: aws.String("state"), Values: aws.StringSlice([]string{StateAvailable})}},
	}, testAccount)
	require.NoError(t, err)
	require.Len(t, out.TransitGateways, 1)
	assert.Equal(t, a, aws.StringValue(out.TransitGateways[0].TransitGatewayId))

	_, err = f.svc.DescribeTransitGateways(t.Context(), &ec2.DescribeTransitGatewaysInput{
		Filters: []*ec2.Filter{{Name: aws.String("bogus-filter"), Values: aws.StringSlice([]string{"x"})}},
	}, testAccount)
	assert.Equal(t, "Value (bogus-filter) for parameter Filter is invalid.", requireCode(t, err, awserrors.ErrorInvalidParameterValue))

	out, err = f.svc.DescribeTransitGateways(t.Context(), &ec2.DescribeTransitGatewaysInput{
		TransitGatewayIds: aws.StringSlice([]string{a}), MaxResults: aws.Int64(5),
	}, testAccount)
	require.NoError(t, err, "MaxResults with IDs is accepted")
	assert.Len(t, out.TransitGateways, 1)

	_, err = f.svc.DescribeTransitGateways(t.Context(), &ec2.DescribeTransitGatewaysInput{
		TransitGatewayIds: aws.StringSlice([]string{"tgw-0123456789abcdef0"}),
	}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidTransitGatewayIDNotFound)

	other, err := f.svc.DescribeTransitGateways(t.Context(), &ec2.DescribeTransitGatewaysInput{}, "210987654321")
	require.NoError(t, err)
	assert.Empty(t, other.TransitGateways, "another account sees none")
}
