package vpcpeering

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	peerAccount  = "210987654321"
	thirdAccount = "333333333333"
	peerVpc      = "vpc-0d000000000000004"
	thirdVpc     = "vpc-0e000000000000005"
)

func newCrossAccountFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.putVPC(t, peerAccount, peerVpc, "10.4.0.0/16")
	f.putVPC(t, thirdAccount, thirdVpc, "10.5.0.0/16")
	return f
}

func (f *fixture) createTo(t *testing.T, owner, vpc, peerOwner, peer string) *ec2.VpcPeeringConnection {
	t.Helper()
	out, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{
		VpcId: aws.String(vpc), PeerVpcId: aws.String(peer), PeerOwnerId: aws.String(peerOwner),
	}, owner)
	require.NoError(t, err)
	return out.VpcPeeringConnection
}

// statusAs is status as viewer sees it, or "" when it is not visible.
func (f *fixture) statusAs(t *testing.T, viewer, id string) string {
	t.Helper()
	out, err := f.svc.DescribeVpcPeeringConnections(t.Context(),
		&ec2.DescribeVpcPeeringConnectionsInput{VpcPeeringConnectionIds: aws.StringSlice([]string{id})}, viewer)
	if awserrors.IsErrorCode(err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound) {
		return ""
	}
	require.NoError(t, err)
	require.Len(t, out.VpcPeeringConnections, 1)
	return aws.StringValue(out.VpcPeeringConnections[0].Status.Code)
}

func (f *fixture) acceptAs(t *testing.T, account, id string) error {
	_, err := f.svc.AcceptVpcPeeringConnection(t.Context(), &ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)}, account)
	return err
}

func (f *fixture) termKeys(t *testing.T) map[string]bool {
	t.Helper()
	keys, err := kvstore.Keys(t.Context(), f.svc.term)
	require.NoError(t, err)
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

func TestCrossAccount_CreateToAnOwnedPeerVpcIsPendingForBoth(t *testing.T) {
	f := newCrossAccountFixture(t)
	pcx := f.createTo(t, testAccount, vpcA, peerAccount, peerVpc)
	id := aws.StringValue(pcx.VpcPeeringConnectionId)
	assert.Equal(t, StateInitiatingRequest, aws.StringValue(pcx.Status.Code))
	assert.Equal(t, "Initiating Request to "+peerAccount, aws.StringValue(pcx.Status.Message))
	assert.Equal(t, peerAccount, aws.StringValue(pcx.AccepterVpcInfo.OwnerId))

	assert.Equal(t, StatePendingAcceptance, f.statusAs(t, testAccount, id))
	assert.Equal(t, StatePendingAcceptance, f.statusAs(t, peerAccount, id))
	assert.Empty(t, f.statusAs(t, thirdAccount, id))
	assert.True(t, f.has(t, acctKey(peerAccount, id)), "the accepter lists the request")
}

// A missing VPC, one another account owns and a malformed owner all fail
// alike, so Create cannot probe another account.
func TestCrossAccount_CreateFailsAlikeWithoutRevealingThePeer(t *testing.T) {
	cases := []struct{ name, owner, peer string }{
		{"absent from the peer account", peerAccount, vpcID(77)},
		{"owned by a third account", peerAccount, thirdVpc},
		{"malformed owner", "not-an-account", peerVpc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossAccountFixture(t)
			before := f.liveKeys(t)
			pcx := f.createTo(t, testAccount, vpcA, tc.owner, tc.peer)
			id := aws.StringValue(pcx.VpcPeeringConnectionId)
			assert.Equal(t, StateInitiatingRequest, aws.StringValue(pcx.Status.Code))

			conns, err := f.describe(t, id)
			require.NoError(t, err)
			assert.Equal(t, StateFailed, aws.StringValue(conns[0].Status.Code))
			assert.Equal(t, failedMessage, aws.StringValue(conns[0].Status.Message))
			assert.Equal(t, before, f.liveKeys(t), "a failed request writes nothing live")
			assert.Empty(t, f.statusAs(t, tc.owner, id))
			for k := range f.termKeys(t) {
				assert.False(t, strings.HasPrefix(k, "acct.") && !strings.HasPrefix(k, "acct."+testAccount+"."),
					"a failed request indexes only its requester: %s", k)
			}
		})
	}
}

func TestCrossAccount_MissingPeerWithinOneAccountStaysSynchronous(t *testing.T) {
	f := newCrossAccountFixture(t)
	_, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{
		VpcId: aws.String(vpcA), PeerVpcId: aws.String(peerVpc), PeerOwnerId: aws.String(testAccount),
	}, testAccount)
	requireMessage(t, err, awserrors.ErrorInvalidVpcIDNotFound, "The vpc ID '"+peerVpc+"' does not exist")
}

func TestCrossAccount_OnlyTheAccepterAcceptsOrRejects(t *testing.T) {
	f := newCrossAccountFixture(t)
	id := aws.StringValue(f.createTo(t, testAccount, vpcA, peerAccount, peerVpc).VpcPeeringConnectionId)
	for _, account := range []string{testAccount, thirdAccount} {
		requireCode(t, f.acceptAs(t, account, id), awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound)
		_, err := f.svc.RejectVpcPeeringConnection(t.Context(), &ec2.RejectVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)}, account)
		requireCode(t, err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound)
	}
	require.NoError(t, f.acceptAs(t, peerAccount, id))
	assert.Equal(t, StateProvisioning, f.statusAs(t, testAccount, id))

	other := aws.StringValue(f.createTo(t, testAccount, vpcB, peerAccount, peerVpc).VpcPeeringConnectionId)
	_, err := f.svc.RejectVpcPeeringConnection(t.Context(), &ec2.RejectVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(other)}, peerAccount)
	require.NoError(t, err)
	assert.Equal(t, StateRejected, f.statusAs(t, testAccount, other))
}

func TestCrossAccount_ThirdAccountCannotChangeOrRouteToIt(t *testing.T) {
	f := newCrossAccountFixture(t)
	id := aws.StringValue(f.createTo(t, testAccount, vpcA, peerAccount, peerVpc).VpcPeeringConnectionId)
	require.NoError(t, f.acceptAs(t, peerAccount, id))
	require.NoError(t, f.svc.MarkActive(t.Context(), id, f.liveRecord(t, id).Generation))

	all, err := f.svc.DescribeVpcPeeringConnections(t.Context(), &ec2.DescribeVpcPeeringConnectionsInput{}, thirdAccount)
	require.NoError(t, err)
	assert.Empty(t, all.VpcPeeringConnections)
	_, err = f.svc.DeleteVpcPeeringConnection(t.Context(), &ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)}, thirdAccount)
	requireCode(t, err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound)
	_, err = f.svc.ModifyVpcPeeringConnectionOptions(t.Context(), &ec2.ModifyVpcPeeringConnectionOptionsInput{
		VpcPeeringConnectionId:            aws.String(id),
		RequesterPeeringConnectionOptions: &ec2.PeeringConnectionOptionsRequest{AllowDnsResolutionFromRemoteVpc: aws.Bool(false)},
	}, thirdAccount)
	requireCode(t, err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound)

	target, err := f.svc.RouteTarget(t.Context(), thirdAccount, id)
	require.NoError(t, err)
	assert.Nil(t, target)
	target, err = f.svc.RouteTarget(t.Context(), peerAccount, id)
	require.NoError(t, err)
	require.NotNil(t, target, "the accepter routes to the connection")
	assert.Equal(t, peerVpc, target.AccepterVpcID)

	_, err = f.svc.ModifyVpcPeeringConnectionOptions(t.Context(), &ec2.ModifyVpcPeeringConnectionOptionsInput{
		VpcPeeringConnectionId:            aws.String(id),
		RequesterPeeringConnectionOptions: &ec2.PeeringConnectionOptionsRequest{AllowDnsResolutionFromRemoteVpc: aws.Bool(false)},
	}, peerAccount)
	requireCode(t, err, awserrors.ErrorOperationNotPermitted)

	_, err = f.svc.DeleteVpcPeeringConnection(t.Context(), &ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)}, peerAccount)
	require.NoError(t, err, "either owner deletes an active connection")
	assert.Equal(t, StateDeleted, f.statusAs(t, testAccount, id))
}

func TestCrossAccount_EachOwnerSeesOnlyItsTags(t *testing.T) {
	f := newCrossAccountFixture(t)
	id := aws.StringValue(f.createTo(t, testAccount, vpcA, peerAccount, peerVpc).VpcPeeringConnectionId)
	for account, value := range map[string]string{testAccount: "requester", peerAccount: "accepter", thirdAccount: "unrelated"} {
		require.NoError(t, f.svc.ApplyRecordTags(&ec2.CreateTagsInput{
			Resources: aws.StringSlice([]string{id}), Tags: []*ec2.Tag{{Key: aws.String("side"), Value: aws.String(value)}},
		}, account))
	}
	for account, want := range map[string]string{testAccount: "requester", peerAccount: "accepter"} {
		out, err := f.svc.DescribeVpcPeeringConnections(t.Context(), &ec2.DescribeVpcPeeringConnectionsInput{
			Filters: []*ec2.Filter{{Name: aws.String("tag:side"), Values: aws.StringSlice([]string{want})}},
		}, account)
		require.NoError(t, err)
		require.Len(t, out.VpcPeeringConnections, 1, account)
		require.Len(t, out.VpcPeeringConnections[0].Tags, 1, account)
		assert.Equal(t, want, aws.StringValue(out.VpcPeeringConnections[0].Tags[0].Value), account)
	}
	_, unrelated := f.liveRecord(t, id).Tags[thirdAccount]
	assert.False(t, unrelated, "an account that owns neither VPC writes no tag")
}

func TestCrossAccount_DeleteVpcRefusedForTheAccepterOnly(t *testing.T) {
	f := newCrossAccountFixture(t)
	id := aws.StringValue(f.createTo(t, testAccount, vpcA, peerAccount, peerVpc).VpcPeeringConnectionId)

	blocked, err := f.svc.VpcBlocksDeletion(t.Context(), peerVpc)
	require.NoError(t, err)
	assert.True(t, blocked, "a request from another account blocks the accepter's DeleteVpc")
	blocked, err = f.svc.VpcBlocksDeletion(t.Context(), vpcA)
	require.NoError(t, err)
	assert.False(t, blocked, "the requester may delete its VPC")

	f.deleteVPC(t, testAccount, vpcA)
	for _, viewer := range []string{testAccount, peerAccount} {
		out, err := f.svc.DescribeVpcPeeringConnections(t.Context(),
			&ec2.DescribeVpcPeeringConnectionsInput{VpcPeeringConnectionIds: aws.StringSlice([]string{id})}, viewer)
		require.NoError(t, err)
		assert.Equal(t, StateDeleted, aws.StringValue(out.VpcPeeringConnections[0].Status.Code), viewer)
		assert.Equal(t, "Deleted by "+testAccount, aws.StringValue(out.VpcPeeringConnections[0].Status.Message), viewer)
	}
	blocked, err = f.svc.VpcBlocksDeletion(t.Context(), peerVpc)
	require.NoError(t, err)
	assert.False(t, blocked, "a request whose requester VPC is gone no longer blocks")
	requireCode(t, f.acceptAs(t, peerAccount, id), awserrors.ErrorInvalidStateTransition)

	_, err = f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, StateDeleted, f.termRecord(t, id).Status)
	f.clock.advance(terminalShortWindow)
	assert.Empty(t, f.statusAs(t, testAccount, id), "the deleter sees it for 2 hours")
	assert.Equal(t, StateDeleted, f.statusAs(t, peerAccount, id), "the other owner sees it for 2 days")
	f.clock.advance(terminalLongWindow - terminalShortWindow)
	assert.Empty(t, f.statusAs(t, peerAccount, id))
}

func TestCrossAccount_PendingWithinOneAccountNeverBlocksDeleteVpc(t *testing.T) {
	f := newCrossAccountFixture(t)
	f.create(t, vpcA, vpcB)
	for _, vpc := range []string{vpcA, vpcB} {
		blocked, err := f.svc.VpcBlocksDeletion(t.Context(), vpc)
		require.NoError(t, err)
		assert.False(t, blocked, vpc)
	}
}
