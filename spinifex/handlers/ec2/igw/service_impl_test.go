package handlers_ec2_igw

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestIGWService(t *testing.T) (*IGWServiceImpl, *nats.Conn) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)

	// Create VPC KV bucket and register test VPCs so fail-closed ownership checks pass
	vpcEntries := map[string][]byte{}
	for _, vpcID := range []string{"vpc-test123", "vpc-other", "vpc-lifecycle", "vpc-event-test"} {
		vpcEntries[utils.AccountKey(testAccountID, vpcID)] = []byte(`{"vpc_id":"` + vpcID + `","state":"available"}`)
	}
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, vpcEntries)

	svc, err := NewIGWServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	return svc, nc
}

func createTestIGW(t *testing.T, svc *IGWServiceImpl) string {
	t.Helper()
	out, err := svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}, testAccountID)
	require.NoError(t, err)
	return *out.InternetGateway.InternetGatewayId
}

func TestCreateInternetGateway(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	out, err := svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.InternetGateway)
	assert.Equal(t, "igw-", (*out.InternetGateway.InternetGatewayId)[:4])
	// Should not have attachments when created
	assert.Empty(t, out.InternetGateway.Attachments)
}

func TestCreateInternetGateway_WithTags(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	out, err := svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("internet-gateway"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-igw")},
					{Key: aws.String("Env"), Value: aws.String("test")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.InternetGateway.Tags, 2)

	// Verify tags persist through describe
	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		InternetGatewayIds: []*string{out.InternetGateway.InternetGatewayId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.InternetGateways, 1)
	assert.Len(t, desc.InternetGateways[0].Tags, 2)
}

func TestCreateInternetGateway_TagsWrongResourceType(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	out, err := svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("wrong-type")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.InternetGateway.Tags)
}

func TestDeleteInternetGateway(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	_, err := svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		InternetGatewayIds: []*string{aws.String(igwID)},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidInternetGatewayID.NotFound")
}

func TestDeleteInternetGateway_NotFound(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	_, err := svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String("igw-nonexistent"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidInternetGatewayID.NotFound")
}

func TestDeleteInternetGateway_MissingID(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	_, err := svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestDeleteInternetGateway_EmptyID(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	_, err := svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(""),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestDeleteInternetGateway_WhileAttached(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	// Attach to a VPC
	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	// Try to delete — should fail with DependencyViolation
	_, err = svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
	}, testAccountID)
	assert.ErrorContains(t, err, "DependencyViolation")
}

func TestDescribeInternetGateways_All(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	createTestIGW(t, svc)
	createTestIGW(t, svc)

	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.InternetGateways, 2)
}

func TestDescribeInternetGateways_ByID(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)
	createTestIGW(t, svc) // second one should be filtered out

	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		InternetGatewayIds: []*string{aws.String(igwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.InternetGateways, 1)
	assert.Equal(t, igwID, *desc.InternetGateways[0].InternetGatewayId)
}

func TestDescribeInternetGateways_Empty(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.InternetGateways)
}

func TestAttachInternetGateway(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	// Verify attachment via describe
	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		InternetGatewayIds: []*string{aws.String(igwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.InternetGateways, 1)
	require.Len(t, desc.InternetGateways[0].Attachments, 1)
	assert.Equal(t, "vpc-test123", *desc.InternetGateways[0].Attachments[0].VpcId)
	assert.Equal(t, "available", *desc.InternetGateways[0].Attachments[0].State)
}

func TestAttachInternetGateway_NotFound(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String("igw-nonexistent"),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidInternetGatewayID.NotFound")
}

func TestAttachInternetGateway_AlreadyAttached(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	// Try attaching again — should fail
	_, err = svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-other"),
	}, testAccountID)
	assert.ErrorContains(t, err, "Resource.AlreadyAssociated")
}

func TestAttachInternetGateway_MissingParams(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		VpcId: aws.String("vpc-test123"),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")

	_, err = svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String("igw-test"),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestDetachInternetGateway(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	// Attach first
	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	// Detach
	_, err = svc.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	// Verify detached
	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		InternetGatewayIds: []*string{aws.String(igwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.InternetGateways, 1)
	assert.Empty(t, desc.InternetGateways[0].Attachments)
}

func TestDetachInternetGateway_NotAttached(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	_, err := svc.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	assert.ErrorContains(t, err, "Gateway.NotAttached")
}

func TestDetachInternetGateway_WrongVPC(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	// Try detaching from wrong VPC
	_, err = svc.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-wrong"),
	}, testAccountID)
	assert.ErrorContains(t, err, "Gateway.NotAttached")
}

func TestDetachInternetGateway_NotFound(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	_, err := svc.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String("igw-nonexistent"),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidInternetGatewayID.NotFound")
}

func TestDetachInternetGateway_MissingParams(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	_, err := svc.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		VpcId: aws.String("vpc-test123"),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")

	_, err = svc.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String("igw-test"),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestIGWLifecycle_CreateAttachDetachDelete(t *testing.T) {
	svc, _ := setupTestIGWService(t)

	// Create
	igwID := createTestIGW(t, svc)

	// Attach
	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-lifecycle"),
	}, testAccountID)
	require.NoError(t, err)

	// Cannot delete while attached
	_, err = svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
	}, testAccountID)
	assert.ErrorContains(t, err, "DependencyViolation")

	// Detach
	_, err = svc.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-lifecycle"),
	}, testAccountID)
	require.NoError(t, err)

	// Now delete succeeds
	_, err = svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
	}, testAccountID)
	require.NoError(t, err)

	// Verify gone
	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.InternetGateways)
}

func TestAttachInternetGateway_PublishesEvent(t *testing.T) {
	svc, nc := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	// Subscribe to IGW attach events
	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.igw-attach", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-event-test"),
	}, testAccountID)
	require.NoError(t, err)

	// Verify event was published
	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), igwID)
		assert.Contains(t, string(msg.Data), "vpc-event-test")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for IGW attach event")
	}
}

func TestDetachInternetGateway_PublishesEvent(t *testing.T) {
	svc, nc := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	// Attach first
	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-event-test"),
	}, testAccountID)
	require.NoError(t, err)

	// Subscribe to IGW detach events
	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.igw-detach", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-event-test"),
	}, testAccountID)
	require.NoError(t, err)

	// Verify event was published
	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), igwID)
		assert.Contains(t, string(msg.Data), "vpc-event-test")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for IGW detach event")
	}
}

func TestCreateInternetGateway_PublishesNoEvent(t *testing.T) {
	svc, nc := setupTestIGWService(t)

	// Subscribe to all vpc.* topics
	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.>", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}, testAccountID)
	require.NoError(t, err)

	// Verify no event was published
	select {
	case msg := <-eventCh:
		t.Fatalf("unexpected event on topic %s", msg.Subject)
	case <-time.After(200 * time.Millisecond):
		// Expected — no event
	}
}

// TestAttachInternetGateway_CrossAccountVPCRejected tests that attaching an IGW to another account's VPC is rejected.
func TestAttachInternetGateway_CrossAccountVPCRejected(t *testing.T) {
	svc, nc := setupTestIGWService(t)

	// Create VPC KV bucket and add a VPC owned by testAccountID
	js, err := nc.JetStream()
	require.NoError(t, err)
	vpcKV, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: handlers_ec2_vpc.KVBucketVPCs, History: 1})
	require.NoError(t, err)

	vpcID := "vpc-alpha123"
	_, err = vpcKV.Put(utils.AccountKey(testAccountID, vpcID), []byte(`{"vpc_id":"vpc-alpha123","state":"available"}`))
	require.NoError(t, err)

	// Refresh service to pick up the VPC KV bucket
	svc.vpcKV = vpcKV

	// Create IGW owned by otherAccountID
	otherAccount := "999999999999"
	out, err := svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{}, otherAccount)
	require.NoError(t, err)
	igwID := *out.InternetGateway.InternetGatewayId

	// Other account tries to attach their IGW to testAccountID's VPC — should fail
	_, err = svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	}, otherAccount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidVpcID.NotFound")

	// Owner attaches their own IGW to their own VPC — should succeed
	ownIGW := createTestIGW(t, svc)
	_, err = svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(ownIGW),
		VpcId:             aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)
}

func TestDescribeInternetGateways_FilterByIGWId(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)
	createTestIGW(t, svc)

	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("internet-gateway-id"), Values: []*string{aws.String(igwID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.InternetGateways, 1)
	assert.Equal(t, igwID, *desc.InternetGateways[0].InternetGatewayId)
}

func TestDescribeInternetGateways_FilterByAttachmentVpcId(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)
	createTestIGW(t, svc) // detached

	// Attach first IGW
	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.vpc-id"), Values: []*string{aws.String("vpc-test123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.InternetGateways, 1)
	assert.Equal(t, igwID, *desc.InternetGateways[0].InternetGatewayId)
}

func TestDescribeInternetGateways_FilterByAttachmentState(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)
	createTestIGW(t, svc) // detached, won't match

	// Attach
	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.state"), Values: []*string{aws.String("available")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.InternetGateways, 1)
	assert.Equal(t, igwID, *desc.InternetGateways[0].InternetGatewayId)
}

func TestDescribeInternetGateways_FilterMultipleValues_OR(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID1 := createTestIGW(t, svc)
	igwID2 := createTestIGW(t, svc)
	createTestIGW(t, svc)

	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("internet-gateway-id"), Values: []*string{aws.String(igwID1), aws.String(igwID2)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.InternetGateways, 2)
}

func TestDescribeInternetGateways_FilterMultipleFilters_AND(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)
	createTestIGW(t, svc) // detached

	_, err := svc.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)

	// Match both
	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("internet-gateway-id"), Values: []*string{aws.String(igwID)}},
			{Name: aws.String("attachment.vpc-id"), Values: []*string{aws.String("vpc-test123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.InternetGateways, 1)

	// Mismatch
	desc, err = svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("internet-gateway-id"), Values: []*string{aws.String(igwID)}},
			{Name: aws.String("attachment.vpc-id"), Values: []*string{aws.String("vpc-wrong")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.InternetGateways)
}

func TestDescribeInternetGateways_FilterUnknownName_Error(t *testing.T) {
	svc, _ := setupTestIGWService(t)

	_, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
		},
	}, testAccountID)
	assert.Error(t, err)
}

func TestDescribeInternetGateways_FilterWildcard(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("internet-gateway-id"), Values: []*string{aws.String("igw-*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.InternetGateways, 1)
	assert.Equal(t, igwID, *desc.InternetGateways[0].InternetGatewayId)
}

func TestDescribeInternetGateways_FilterNoResults(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	createTestIGW(t, svc)

	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("internet-gateway-id"), Values: []*string{aws.String("igw-nonexistent")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.InternetGateways)
}

func TestDescribeInternetGateways_FilterByTag(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	out, err := svc.CreateInternetGateway(&ec2.CreateInternetGatewayInput{
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("internet-gateway"),
				Tags:         []*ec2.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	createTestIGW(t, svc) // untagged

	desc, err := svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Env"), Values: []*string{aws.String("prod")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.InternetGateways, 1)
	assert.Equal(t, *out.InternetGateway.InternetGatewayId, *desc.InternetGateways[0].InternetGatewayId)
}

func TestDeleteInternetGateway_PublishesNoEvent(t *testing.T) {
	svc, nc := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	// Subscribe to all vpc.* topics
	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.>", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
	}, testAccountID)
	require.NoError(t, err)

	// Verify no event was published
	select {
	case msg := <-eventCh:
		t.Fatalf("unexpected event on topic %s", msg.Subject)
	case <-time.After(200 * time.Millisecond):
		// Expected — no event
	}
}
