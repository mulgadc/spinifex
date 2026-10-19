package spotinstance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Open and active requests contribute their key and group names; cancelled
// requests and other accounts' requests do not.
func TestAccountLaunchReferences_ReadsOpenAndActiveRequests(t *testing.T) {
	svc := setupTestService(t)
	active := makeSIR("sir-active01", "i-0000000000000001")
	active.LaunchSpecification.Placement = &ec2.SpotPlacement{GroupName: aws.String("pg-spread")}
	open := makeSIR("sir-open0001", "")
	open.State = aws.String(ec2.SpotInstanceStateOpen)
	open.LaunchSpecification.KeyName = aws.String("open-key")
	putSIRs(t, svc, active, open)
	cancelled := makeSIR("sir-cancel01", "i-0000000000000002")
	cancelled.LaunchSpecification.KeyName = aws.String("cancelled-key")
	putSIRs(t, svc, cancelled)
	_, err := svc.CancelSpotInstanceRequests(t.Context(), &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-cancel01")},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.PutSpotInstanceRequests(t.Context(), &PutSpotRequestsInput{Requests: []*ec2.SpotInstanceRequest{makeSIR("sir-other001", "")}}, "999988887777")
	require.NoError(t, err)

	refs, err := svc.AccountLaunchReferences(t.Context(), testAccountID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"my-key", "open-key"}, refs.KeyNames)
	assert.Equal(t, []string{"pg-spread"}, refs.PlacementGroupNames)
}

// An unreadable request may name anything, so the check fails rather than
// answer without it.
func TestAccountLaunchReferences_UnreadableRequestFailsTheCheck(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-active01", "i-0000000000000001"))
	_, err := svc.activeKV.Put(t.Context(), kvutil.AccountKey(testAccountID, "sir-broken01"), []byte("{not json"))
	require.NoError(t, err)

	_, err = svc.AccountLaunchReferences(t.Context(), testAccountID)
	require.Error(t, err)
}
