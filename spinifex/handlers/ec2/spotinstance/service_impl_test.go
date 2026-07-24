package handlers_ec2_spotinstance

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func setupTestService(t *testing.T) *SpotInstanceServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	svc, err := NewSpotInstanceServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	return svc
}

func TestTerminalBucketTTL(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	_, err := NewSpotInstanceServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(t.Context(), KVBucketSpotRequestsTerminal)
	require.NoError(t, err)
	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, spotTerminalTTL, status.TTL())
}

func TestGetOrCreateTerminalBucketPreservesExistingConfiguration(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	const existingTTL = 2 * time.Hour

	_, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  KVBucketSpotRequestsTerminal,
		History: 1,
		TTL:     existingTTL,
	})
	require.NoError(t, err)

	kv, err := getOrCreateTerminalBucket(t.Context(), js)
	require.NoError(t, err)
	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, existingTTL, status.TTL())
}

// makeSIR builds an active/fulfilled Spot Instance Request for tests.
func makeSIR(sirID, instanceID string) *ec2.SpotInstanceRequest {
	return &ec2.SpotInstanceRequest{
		SpotInstanceRequestId:    aws.String(sirID),
		InstanceId:               aws.String(instanceID),
		State:                    aws.String(ec2.SpotInstanceStateActive),
		Type:                     aws.String(ec2.SpotInstanceTypeOneTime),
		LaunchedAvailabilityZone: aws.String("ap-southeast-2a"),
		Status:                   &ec2.SpotInstanceStatus{Code: aws.String(SpotStatusCodeFulfilled)},
		LaunchSpecification: &ec2.LaunchSpecification{
			ImageId:      aws.String("ami-12345678"),
			InstanceType: aws.String("t3.micro"),
			KeyName:      aws.String("my-key"),
		},
	}
}

func putSIRs(t *testing.T, svc *SpotInstanceServiceImpl, reqs ...*ec2.SpotInstanceRequest) {
	t.Helper()
	_, err := svc.PutSpotInstanceRequests(t.Context(), &PutSpotRequestsInput{Requests: reqs}, testAccountID)
	require.NoError(t, err)
}

func describeAll(t *testing.T, svc *SpotInstanceServiceImpl, accountID string) []*ec2.SpotInstanceRequest {
	t.Helper()
	out, err := svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{}, accountID)
	require.NoError(t, err)
	return out.SpotInstanceRequests
}

// --- Put / Describe round-trip ---

func TestPutDescribe_RoundTrip(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"), makeSIR("sir-bbb", "i-bbb"))

	got := describeAll(t, svc, testAccountID)
	require.Len(t, got, 2)

	ids := map[string]string{}
	for _, r := range got {
		ids[aws.StringValue(r.SpotInstanceRequestId)] = aws.StringValue(r.InstanceId)
		assert.Equal(t, ec2.SpotInstanceStateActive, aws.StringValue(r.State))
	}
	assert.Equal(t, "i-aaa", ids["sir-aaa"])
	assert.Equal(t, "i-bbb", ids["sir-bbb"])
}

func TestPutSpotInstanceRequests_MissingID(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.PutSpotInstanceRequests(t.Context(), &PutSpotRequestsInput{
		Requests: []*ec2.SpotInstanceRequest{{InstanceId: aws.String("i-noid")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDescribe_Empty(t *testing.T) {
	svc := setupTestService(t)
	assert.Empty(t, describeAll(t, svc, testAccountID))
}

func TestDescribe_AccountScoped(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"))

	assert.Len(t, describeAll(t, svc, testAccountID), 1)
	assert.Empty(t, describeAll(t, svc, "999999999999"))
}

// --- Describe filters ---

func TestDescribe_FilterByID(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"), makeSIR("sir-bbb", "i-bbb"))

	out, err := svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-aaa")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SpotInstanceRequests, 1)
	assert.Equal(t, "sir-aaa", aws.StringValue(out.SpotInstanceRequests[0].SpotInstanceRequestId))
}

func TestDescribe_UnknownID(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"))

	_, err := svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-ghost")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidSpotInstanceRequestIDNotFound, err.Error())
}

func TestDescribe_FilterByState(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"), makeSIR("sir-bbb", "i-bbb"))

	// Cancel one so it becomes terminal/cancelled.
	_, err := svc.CancelSpotInstanceRequests(t.Context(), &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-bbb")},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []*ec2.Filter{{Name: aws.String("state"), Values: []*string{aws.String("active")}}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SpotInstanceRequests, 1)
	assert.Equal(t, "sir-aaa", aws.StringValue(out.SpotInstanceRequests[0].SpotInstanceRequestId))
}

func TestDescribe_FilterByInstanceID(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"), makeSIR("sir-bbb", "i-bbb"))

	out, err := svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []*ec2.Filter{{Name: aws.String("instance-id"), Values: []*string{aws.String("i-bbb")}}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SpotInstanceRequests, 1)
	assert.Equal(t, "sir-bbb", aws.StringValue(out.SpotInstanceRequests[0].SpotInstanceRequestId))
}

func TestDescribe_FilterByLaunchImageID(t *testing.T) {
	svc := setupTestService(t)
	other := makeSIR("sir-bbb", "i-bbb")
	other.LaunchSpecification.ImageId = aws.String("ami-99999999")
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"), other)

	out, err := svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []*ec2.Filter{{Name: aws.String("launch.image-id"), Values: []*string{aws.String("ami-12345678")}}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SpotInstanceRequests, 1)
	assert.Equal(t, "sir-aaa", aws.StringValue(out.SpotInstanceRequests[0].SpotInstanceRequestId))
}

func TestDescribe_FilterByTag(t *testing.T) {
	svc := setupTestService(t)
	tagged := makeSIR("sir-aaa", "i-aaa")
	tagged.Tags = []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}}
	putSIRs(t, svc, tagged, makeSIR("sir-bbb", "i-bbb"))

	out, err := svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []*ec2.Filter{{Name: aws.String("tag:env"), Values: []*string{aws.String("prod")}}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SpotInstanceRequests, 1)
	assert.Equal(t, "sir-aaa", aws.StringValue(out.SpotInstanceRequests[0].SpotInstanceRequestId))

	// tag-key matches any value for the key.
	out, err = svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []*ec2.Filter{{Name: aws.String("tag-key"), Values: []*string{aws.String("env")}}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SpotInstanceRequests, 1)
	assert.Equal(t, "sir-aaa", aws.StringValue(out.SpotInstanceRequests[0].SpotInstanceRequestId))
}

func TestDescribe_InvalidFilter(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []*ec2.Filter{{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}}},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

// --- Cancel ---

func TestCancel_MovesActiveToTerminal(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"))

	out, err := svc.CancelSpotInstanceRequests(t.Context(), &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-aaa")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.CancelledSpotInstanceRequests, 1)
	assert.Equal(t, ec2.CancelSpotInstanceRequestStateCancelled, aws.StringValue(out.CancelledSpotInstanceRequests[0].State))

	// Active bucket no longer has it; describe (merged) shows it cancelled with instance still set.
	got := describeAll(t, svc, testAccountID)
	require.Len(t, got, 1)
	assert.Equal(t, ec2.SpotInstanceStateCancelled, aws.StringValue(got[0].State))
	assert.Equal(t, SpotStatusCodeCanceledInstanceRunning, aws.StringValue(got[0].Status.Code))
	assert.Equal(t, "i-aaa", aws.StringValue(got[0].InstanceId))
}

func TestCancel_Idempotent(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"))

	in := &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: []*string{aws.String("sir-aaa")}}
	_, err := svc.CancelSpotInstanceRequests(t.Context(), in, testAccountID)
	require.NoError(t, err)

	// Second cancel of an already-terminal request succeeds without duplicating.
	out, err := svc.CancelSpotInstanceRequests(t.Context(), in, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.CancelledSpotInstanceRequests, 1)

	got := describeAll(t, svc, testAccountID)
	require.Len(t, got, 1)
	assert.Equal(t, ec2.SpotInstanceStateCancelled, aws.StringValue(got[0].State))
}

func TestCancel_AbsentIDIdempotent(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CancelSpotInstanceRequests(t.Context(), &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-ghost")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.CancelledSpotInstanceRequests, 1)
	assert.Equal(t, ec2.CancelSpotInstanceRequestStateCancelled, aws.StringValue(out.CancelledSpotInstanceRequests[0].State))
}

func TestCancel_MissingIDs(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CancelSpotInstanceRequests(t.Context(), &ec2.CancelSpotInstanceRequestsInput{}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

// --- CloseForInstance ---

func TestCloseForInstance_MovesMatching(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"), makeSIR("sir-bbb", "i-bbb"))

	require.NoError(t, svc.CloseForInstance(t.Context(), "i-bbb", testAccountID))

	out, err := svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-bbb")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SpotInstanceRequests, 1)
	assert.Equal(t, ec2.SpotInstanceStateClosed, aws.StringValue(out.SpotInstanceRequests[0].State))
	assert.Equal(t, SpotStatusCodeInstanceTerminatedByUser, aws.StringValue(out.SpotInstanceRequests[0].Status.Code))

	// sir-aaa is untouched and still active.
	out, err = svc.DescribeSpotInstanceRequests(t.Context(), &ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-aaa")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, ec2.SpotInstanceStateActive, aws.StringValue(out.SpotInstanceRequests[0].State))
}

func TestCloseForInstance_NoMatchNoOp(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"))

	require.NoError(t, svc.CloseForInstance(t.Context(), "i-nomatch", testAccountID))

	got := describeAll(t, svc, testAccountID)
	require.Len(t, got, 1)
	assert.Equal(t, ec2.SpotInstanceStateActive, aws.StringValue(got[0].State))
}

func TestCloseForInstance_EmptyInstanceID(t *testing.T) {
	svc := setupTestService(t)
	require.NoError(t, svc.CloseForInstance(t.Context(), "", testAccountID))
}

// --- Merge across buckets ---

func TestDescribe_MergesBothBuckets(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-active", "i-1"), makeSIR("sir-cancel", "i-2"), makeSIR("sir-close", "i-3"))

	_, err := svc.CancelSpotInstanceRequests(t.Context(), &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String("sir-cancel")},
	}, testAccountID)
	require.NoError(t, err)
	require.NoError(t, svc.CloseForInstance(t.Context(), "i-3", testAccountID))

	states := map[string]string{}
	for _, r := range describeAll(t, svc, testAccountID) {
		states[aws.StringValue(r.SpotInstanceRequestId)] = aws.StringValue(r.State)
	}
	assert.Equal(t, ec2.SpotInstanceStateActive, states["sir-active"])
	assert.Equal(t, ec2.SpotInstanceStateCancelled, states["sir-cancel"])
	assert.Equal(t, ec2.SpotInstanceStateClosed, states["sir-close"])
}
