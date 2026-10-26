package daemon

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eigw "github.com/mulgadc/spinifex/spinifex/domains/ec2/eigw"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	ec2igw "github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2natgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/natgw"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2snapshot "github.com/mulgadc/spinifex/spinifex/domains/ec2/snapshot"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subscribeOwner registers this daemon as the instance's owner on ec2.cmd.<id>,
// the way daemon startup does for every VM it runs.
func subscribeOwner(t *testing.T, d *Daemon, instanceID string) {
	t.Helper()
	sub, err := d.natsConn.Subscribe("ec2.cmd."+instanceID, d.handleEC2Events)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// A mixed create-tags writes the instance via the owner-gated co-located path
// and the non-instance resource via the generic central write.
func TestCreateTags_MixedResourceDispatch(t *testing.T) {
	const id = "i-tag-mixed"
	d := tagTestDaemon(t, id, map[string]string{"Name": "web"})
	subscribeOwner(t, d, id)

	out, err := d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String(id), aws.String("vol-tag-mixed")},
		Tags:      []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	want := map[string]string{"Name": "web", "env": "prod"}
	assert.Equal(t, want, recordTags(t, d, id))
	assert.Equal(t, want, centralTags(t, d, testAccountID, id))
	assert.Equal(t, map[string]string{"env": "prod"}, centralTags(t, d, testAccountID, "vol-tag-mixed"))
}

// delete-tags routes instance removals via the owner: bare keys delete
// unconditionally, valued keys only on match, and both stores agree after.
func TestDeleteTags_InstanceRoutedViaOwner(t *testing.T) {
	const id = "i-tag-del"
	d := tagTestDaemon(t, id, map[string]string{"Name": "web", "env": "dev", "team": "infra"})
	subscribeOwner(t, d, id)
	require.NoError(t, d.tagsService.PutResourceTags(t.Context(), testAccountID, id,
		map[string]string{"Name": "web", "env": "dev", "team": "infra"}))

	_, err := d.deleteTags(context.Background(), &ec2.DeleteTagsInput{
		Resources: []*string{aws.String(id)},
		Tags: []*ec2.Tag{
			{Key: aws.String("team")},
			{Key: aws.String("env"), Value: aws.String("prod")},
			{Key: aws.String("Name"), Value: aws.String("web")},
		},
	}, testAccountID)
	require.NoError(t, err)

	want := map[string]string{"env": "dev"}
	assert.Equal(t, want, recordTags(t, d, id))
	assert.Equal(t, want, centralTags(t, d, testAccountID, id))
}

// delete-tags with no Tags clears every tag on the instance in both stores.
func TestDeleteTags_InstanceClearAll(t *testing.T) {
	const id = "i-tag-delall"
	d := tagTestDaemon(t, id, map[string]string{"Name": "web", "env": "dev"})
	subscribeOwner(t, d, id)
	require.NoError(t, d.tagsService.PutResourceTags(t.Context(), testAccountID, id,
		map[string]string{"Name": "web", "env": "dev"}))

	_, err := d.deleteTags(context.Background(), &ec2.DeleteTagsInput{
		Resources: []*string{aws.String(id)},
	}, testAccountID)
	require.NoError(t, err)

	assert.Empty(t, recordTags(t, d, id))
	assert.Empty(t, centralTags(t, d, testAccountID, id))
}

// An instance with no responding owner returns InvalidID.NotFound and no
// central entry is created for it, while co-listed resources still land.
func TestCreateTags_NoOwnerNotFound(t *testing.T) {
	const absent = "i-tag-absent"
	d := tagTestDaemon(t, "i-tag-unrelated", nil)

	_, err := d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("vol-tag-still"), aws.String(absent)},
		Tags:      []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())

	assert.Empty(t, centralTags(t, d, testAccountID, absent))
	assert.Equal(t, map[string]string{"env": "prod"}, centralTags(t, d, testAccountID, "vol-tag-still"))
}

// A many-instance call fans out concurrently: absent owners all fail fast
// with NotFound rather than serialising owner requests.
func TestDeleteTags_NoOwnerNotFound(t *testing.T) {
	d := tagTestDaemon(t, "i-tag-unrelated2", nil)

	_, err := d.deleteTags(context.Background(), &ec2.DeleteTagsInput{
		Resources: []*string{aws.String("i-tag-gone1"), aws.String("i-tag-gone2")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// With no running owner, create-tags falls back to the shared stopped store
// and the mutation lands on both the stopped record and the central store.
func TestCreateTags_StoppedFallback(t *testing.T) {
	const id = "i-tag-stopfall"
	d, stopped := tagTestDaemonWithStopped(t, "i-tag-unrelated3", nil)
	stopped.Stopped[id] = &vm.VM{
		ID:        id,
		Status:    vm.StateStopped,
		AccountID: testAccountID,
		Instance: &ec2.Instance{InstanceId: aws.String(id),
			Tags: []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("web")}}},
	}

	_, err := d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String(id)},
		Tags:      []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.NoError(t, err)

	want := map[string]string{"Name": "web", "env": "prod"}
	assert.Equal(t, want, tagsAsMap(stopped.Stopped[id].Instance.Tags))
	assert.Equal(t, want, centralTags(t, d, testAccountID, id))
}

// A stopped instance owned by another account is rejected via the fallback's
// owner check and nothing is written to either store.
func TestDeleteTags_StoppedCrossAccountRejected(t *testing.T) {
	const id = "i-tag-stopcross"
	const attacker = "999999999999"
	d, stopped := tagTestDaemonWithStopped(t, "i-tag-unrelated4", nil)
	stopped.Stopped[id] = &vm.VM{
		ID:        id,
		Status:    vm.StateStopped,
		AccountID: testAccountID,
		Instance: &ec2.Instance{InstanceId: aws.String(id),
			Tags: []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("web")}}},
	}

	_, err := d.deleteTags(context.Background(), &ec2.DeleteTagsInput{
		Resources: []*string{aws.String(id)},
	}, attacker)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())

	assert.Equal(t, map[string]string{"Name": "web"}, tagsAsMap(stopped.Stopped[id].Instance.Tags))
	assert.Empty(t, centralTags(t, d, attacker, id))
}

// A daemon-side error reply from the owner (cross-account NotFound) is
// relayed to the caller and nothing lands in either central namespace.
func TestCreateTags_OwnerErrorRelayed(t *testing.T) {
	const id = "i-tag-crossacct"
	const attacker = "999999999999"
	d := tagTestDaemon(t, id, map[string]string{"Name": "web"})
	subscribeOwner(t, d, id)

	_, err := d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String(id)},
		Tags:      []*ec2.Tag{{Key: aws.String("stolen"), Value: aws.String("yes")}},
	}, attacker)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())

	assert.Equal(t, map[string]string{"Name": "web"}, recordTags(t, d, id))
	assert.Empty(t, centralTags(t, d, testAccountID, id))
	assert.Empty(t, centralTags(t, d, attacker, id))
}

func TestCreateTags_Validation(t *testing.T) {
	d := tagTestDaemon(t, "i-tag-valid", nil)

	_, err := d.createTags(context.Background(), nil, testAccountID)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

	_, err = d.createTags(context.Background(), &ec2.CreateTagsInput{
		Tags: []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-tag-valid")},
	}, testAccountID)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDeleteTags_Validation(t *testing.T) {
	d := tagTestDaemon(t, "i-tag-valid2", nil)

	_, err := d.deleteTags(context.Background(), nil, testAccountID)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

	_, err = d.deleteTags(context.Background(), &ec2.DeleteTagsInput{}, testAccountID)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

// Every service that stores tags on its own record must be mirrored, or a
// create-tags reaches the central store and leaves that record stale.
func TestRecordTagMirrors_IncludesEveryRecordStore(t *testing.T) {
	assert.Empty(t, (&Daemon{eipService: ec2eip.NewDisabledEIPService()}).recordTagMirrors(),
		"the disabled EIP service holds no records to mirror")

	d := &Daemon{
		vpcService:        &ec2vpc.VPCServiceImpl{},
		imageService:      &ec2image.ImageServiceImpl{},
		volumeService:     &ec2volume.VolumeServiceImpl{},
		snapshotService:   &ec2snapshot.SnapshotServiceImpl{},
		routeTableService: &ec2routetable.RouteTableServiceImpl{},
		igwService:        &ec2igw.IGWServiceImpl{},
		eigwService:       &ec2eigw.EgressOnlyIGWServiceImpl{},
		eipService:        &ec2eip.EIPServiceImpl{},
		natGatewayService: &ec2natgw.NatGatewayServiceImpl{},
		keyService:        &ec2key.KeyServiceImpl{},
	}
	assert.Len(t, d.recordTagMirrors(), 10)
}

func TestDeleteTags_NonInstanceResource(t *testing.T) {
	d := tagTestDaemon(t, "i-p1-tag-unused", nil)
	require.NoError(t, d.tagsService.PutResourceTags(t.Context(), testAccountID, "vol-p1-tagdel",
		map[string]string{"env": "dev", "team": "infra"}))

	_, err := d.deleteTags(context.Background(), &ec2.DeleteTagsInput{
		Resources: []*string{nil, aws.String("vol-p1-tagdel")},
		Tags:      []*ec2.Tag{{Key: nil}, {Key: aws.String("team")}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "dev"}, centralTags(t, d, testAccountID, "vol-p1-tagdel"))
}

func TestTags_CentralStoreFailure(t *testing.T) {
	d := tagTestDaemon(t, "i-p1-tag-unused2", nil)
	_, tagKV := faultBucket(t)
	for _, m := range []string{"Get", "Put", "Create", "Update", "Delete", "Purge"} {
		tagKV.setFail(m, true)
	}
	d.tagsService = ec2tags.NewTagsServiceImplWithStore(d.config, objectstore.NewMemoryObjectStore(), tagKV)
	vol := []*string{aws.String("vol-p1-tagfail")}

	_, err := d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: vol, Tags: []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID)
	require.Error(t, err)

	_, err = d.deleteTags(context.Background(), &ec2.DeleteTagsInput{Resources: vol}, testAccountID)
	require.Error(t, err)
}

func TestCreateTags_OwnerRequestFails(t *testing.T) {
	d := tagTestDaemon(t, "i-p1-tag-closed", nil)
	closed, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	closed.Close()
	orig := d.natsConn
	d.natsConn = closed
	defer func() { d.natsConn = orig }()

	_, err = d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-p1-tag-closed")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error(),
		"a transport failure is not evidence the instance is gone")
}
