package handlers_rds

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	iammock "github.com/mulgadc/spinifex/spinifex/handlers/iam/mock"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testDefaultVPC     = "vpc-default01"
	testDefaultVPCCIDR = "10.0.0.0/16"
	testDefaultSG      = "sg-default01"
	testBaseDomain     = "spx3.net"
	testDBInstanceID   = "orders-db"
	// The one zone this platform exposes, which every subnet reports.
	testZone = "spinifexz1"
)

// fakeNetwork is the customer account's VPC as the placement resolver reads it:
// one default VPC with two subnets and a default security group.
type fakeNetwork struct {
	vpcs     []*ec2.Vpc
	subnets  []*ec2.Subnet
	groups   []*ec2.SecurityGroup
	vpcErr   error
	subErr   error
	groupErr error

	// accts records which account each describe was issued against, which is how
	// the cross-account read becomes observable.
	accts []string

	// vpcFilters records the filters DescribeVpcs was called with, so a test can
	// assert the names the resolver sends are ones the real surface accepts.
	vpcFilters []*ec2.Filter
}

var _ networkResolver = (*fakeNetwork)(nil)

func newFakeNetwork() *fakeNetwork {
	return &fakeNetwork{
		vpcs: []*ec2.Vpc{{
			VpcId:     aws.String(testDefaultVPC),
			IsDefault: aws.Bool(true),
			CidrBlock: aws.String(testDefaultVPCCIDR),
		}},
		// Deliberately out of order: the resolver sorts, so the placement is the
		// same on every repeat regardless of how the describe happened to order.
		subnets: []*ec2.Subnet{
			{SubnetId: aws.String("subnet-zebra"), VpcId: aws.String(testDefaultVPC), AvailabilityZone: aws.String(testZone)},
			{SubnetId: aws.String("subnet-alpha"), VpcId: aws.String(testDefaultVPC), AvailabilityZone: aws.String(testZone)},
		},
		groups: []*ec2.SecurityGroup{
			{GroupId: aws.String("sg-app01"), GroupName: aws.String("app")},
			{GroupId: aws.String(testDefaultSG), GroupName: aws.String("default")},
		},
	}
}

// Rejects a filter name the real DescribeVpcs would reject, rather than ignoring
// input filters: a permissive fake here let the camelCase 'isDefault' spelling
// reach production, where the EC2 surface answered InvalidParameterValue.
func (f *fakeNetwork) DescribeVpcs(_ context.Context, input *ec2.DescribeVpcsInput, accountID string) (*ec2.DescribeVpcsOutput, error) {
	f.accts = append(f.accts, accountID)
	if input != nil {
		f.vpcFilters = append(f.vpcFilters, input.Filters...)
		for _, filter := range input.Filters {
			if name := aws.StringValue(filter.Name); !handlers_ec2_vpc.SupportsDescribeVpcsFilter(name) {
				return nil, errors.New(awserrors.ErrorInvalidParameterValue)
			}
		}
	}
	if f.vpcErr != nil {
		return nil, f.vpcErr
	}
	if input == nil || len(input.VpcIds) == 0 {
		return &ec2.DescribeVpcsOutput{Vpcs: f.vpcs}, nil
	}
	wanted := aws.StringValueSlice(input.VpcIds)
	var matched []*ec2.Vpc
	for _, vpc := range f.vpcs {
		if slices.Contains(wanted, aws.StringValue(vpc.VpcId)) {
			matched = append(matched, vpc)
		}
	}
	return &ec2.DescribeVpcsOutput{Vpcs: matched}, nil
}

// Honours SubnetIds the way EC2 does — a named subnet that is not in this
// account simply does not come back — which is what makes a subnet group's
// existence and ownership checks observable.
func (f *fakeNetwork) DescribeSubnets(_ context.Context, input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error) {
	f.accts = append(f.accts, accountID)
	if f.subErr != nil {
		return nil, f.subErr
	}
	if input == nil || len(input.SubnetIds) == 0 {
		return &ec2.DescribeSubnetsOutput{Subnets: f.subnets}, nil
	}
	wanted := aws.StringValueSlice(input.SubnetIds)
	var matched []*ec2.Subnet
	for _, subnet := range f.subnets {
		if slices.Contains(wanted, aws.StringValue(subnet.SubnetId)) {
			matched = append(matched, subnet)
		}
	}
	return &ec2.DescribeSubnetsOutput{Subnets: matched}, nil
}

func (f *fakeNetwork) DescribeSecurityGroups(_ context.Context, _ *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.accts = append(f.accts, accountID)
	if f.groupErr != nil {
		return nil, f.groupErr
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: f.groups}, nil
}

// createHarness is a Service wired to the launch fakes plus a customer VPC, so a
// create runs end to end against KV without touching EC2 or northstar.
type createHarness struct {
	svc     *Service
	launch  *launchHarness
	network *fakeNetwork
	iam     *iammock.SystemInstanceRoleEnsurer
	nc      *nats.Conn

	// dnsChanges collects what the endpoint publish put on the bus.
	dnsChanges chan handlers_dns.ChangeBatch
}

func newCreateHarness(t *testing.T, baseDomain string) *createHarness {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	h := &createHarness{
		launch:     newLaunchHarness(),
		network:    newFakeNetwork(),
		iam:        iammock.New(),
		nc:         nc,
		dnsChanges: make(chan handlers_dns.ChangeBatch, 4),
	}
	// The storage key is configured on a real cluster, so the volume comes back
	// encrypted; the unencrypted case is its own test.
	h.launch.volumes.encrypted = true

	// The DNS writer is a request-reply consumer, so without a responder the
	// best-effort publish would sit out its own timeout on every create.
	h.stubDNSWriter(t)

	h.svc = NewService(nc, testRegion).WithDeps(Deps{
		LoadCA:             newTestCA(t),
		MasterKey:          testMasterKey,
		Launch:             h.launch.deps(),
		Network:            h.network,
		IAM:                testIAMProvider(h.iam),
		BaseDomain:         baseDomain,
		ServingCertKeyBits: testServingCertKeyBits,
	})
	return h
}

func (h *createHarness) stubDNSWriter(t *testing.T) {
	t.Helper()
	sub, err := h.nc.Subscribe(handlers_dns.SubjectRecordsetChange, func(msg *nats.Msg) {
		var batch handlers_dns.ChangeBatch
		if err := json.Unmarshal(msg.Data, &batch); err == nil {
			h.dnsChanges <- batch
		}
		if err := msg.Respond([]byte(`{}`)); err != nil {
			t.Logf("respond on %s: %v", handlers_dns.SubjectRecordsetChange, err)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func (h *createHarness) record(t *testing.T, id string) DBInstanceRecord {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(id), &rec)
	require.NoError(t, err)
	require.True(t, found, "DB instance %s should be stored", id)
	return rec
}

func (h *createHarness) recordExists(t *testing.T, id string) bool {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(id), &rec)
	require.NoError(t, err)
	return found
}

func replaceInstanceRecord(t *testing.T, svc *Service, id string) DBInstanceRecord {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	key := DBInstanceKey(id)
	require.NoError(t, kv.Delete(t.Context(), key))
	replacement := DBInstanceRecord{
		DBInstanceIdentifier: id,
		DbiResourceID:        "db-replacement-owner",
		InstanceID:           "i-replacement",
		Status:               StatusCreating,
	}
	require.NoError(t, createJSON(t.Context(), kv, key, &replacement))
	return replacement
}

func validCreateInput() *rds.CreateDBInstanceInput {
	return &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
		Engine:               aws.String("postgres"),
		DBInstanceClass:      aws.String("db.t3.medium"),
		AllocatedStorage:     aws.Int64(20),
		MasterUsername:       aws.String("appuser"),
		MasterUserPassword:   aws.String("Sup3rSecret!"),
		DBName:               aws.String("orders"),
	}
}

func TestCreateDBInstance_ProvisionsAndRecordsTheInstance(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	out, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err)

	// The customer sees a creating instance immediately: the engine is not up
	// yet, and the reconciler owns the flip to available.
	require.NotNil(t, out.DBInstance)
	assert.Equal(t, string(StatusCreating), aws.StringValue(out.DBInstance.DBInstanceStatus))
	assert.Equal(t, "postgres", aws.StringValue(out.DBInstance.Engine))
	assert.Equal(t, FormatARN(ResourceKindDBInstance, testRegion, testAccountID, testDBInstanceID),
		aws.StringValue(out.DBInstance.DBInstanceArn))
	require.NotNil(t, out.DBInstance.Endpoint)
	assert.Equal(t, testDBInstanceID+"."+testAccountID+"."+testRegion+".rds."+testBaseDomain,
		aws.StringValue(out.DBInstance.Endpoint.Address))
	assert.Equal(t, int64(5432), aws.Int64Value(out.DBInstance.Endpoint.Port),
		"the engine's default port applies when the request names none")

	rec := h.record(t, testDBInstanceID)
	assert.Equal(t, StatusCreating, rec.Status)
	assert.Equal(t, "i-rds0001", rec.InstanceID)
	assert.Equal(t, int64(firstVMGeneration), rec.VMGeneration)
	assert.NotEmpty(t, rec.SystemENIID)
	assert.NotEmpty(t, rec.ENIID)
	assert.NotEqual(t, rec.SystemENIID, rec.ENIID, "the system and customer NICs are distinct")
	assert.Equal(t, "vol-rdsdata01", rec.DataVolumeID)
	assert.Equal(t, vm.VolumeSerial(rec.DataVolumeID), rec.DataVolumeSerial)
	assert.True(t, rec.FormatAuthorized, "only the fresh initial-create volume may be formatted")
	assert.True(t, rec.StorageEncrypted)
	// The master password is staged under its own encrypted key, never on the
	// record; TestCreateDBInstance_StagesThePasswordEncrypted covers the payload.
	assert.Equal(t, BootstrapStatePending, rec.Bootstrap.State)
	assert.Empty(t, rec.Bootstrap.MasterUserPassword)

	events := describeEvents(t, h.svc, &rds.DescribeEventsInput{
		SourceType:       aws.String(EventSourceTypeDBInstance),
		SourceIdentifier: aws.String(testDBInstanceID),
	})
	require.Len(t, events, 1)
	assert.Equal(t, "DB instance created.", aws.StringValue(events[0].Message))
	assert.Equal(t, []string{EventCategoryCreation}, aws.StringValueSlice(events[0].EventCategories))

	// The placement is the account's default VPC and its lowest-sorted subnet,
	// with the VPC's default security group when the request names none.
	assert.Equal(t, testDefaultVPC, rec.VpcID)
	assert.Equal(t, "subnet-alpha", rec.SubnetID)
	assert.Equal(t, []string{testDefaultSG}, rec.VpcSecurityGroupIDs)
	for _, acct := range h.network.accts {
		assert.Equal(t, testAccountID, acct, "placement is read in the customer's account")
	}

	// The instance index is what lets an agent's gateway call resolve its own
	// DB instance, so it must exist before the VM can finish booting.
	entry, err := h.svc.LookupInstanceIndex(t.Context(), "i-rds0001")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, testAccountID, entry.AccountID)
	assert.Equal(t, testDBInstanceID, entry.DBInstanceIdentifier)
	assert.Equal(t, int64(firstVMGeneration), entry.VMGeneration)

	require.NotNil(t, h.launch.launcher.input)
	assert.Equal(t, rdsInstanceProfileARN(utils.GlobalAccountID), h.launch.launcher.input.IamInstanceProfileArn)
}

func TestCreateDBInstance_IAMFailurePrecedesReservationAndLaunch(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	iamErr := errors.New("IAM store unavailable")
	h.iam.PutRolePolicyErr = iamErr

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)

	require.Error(t, err)
	assert.ErrorIs(t, err, iamErr)
	assert.False(t, h.recordExists(t, testDBInstanceID))
	assert.Nil(t, h.launch.launcher.input)
	assert.Empty(t, h.launch.enis.created)
	assert.Empty(t, h.launch.volumes.created)
}

func TestCreateDBInstance_PublishesEndpointRecord(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err)

	batch := <-h.dnsChanges
	require.Len(t, batch.Changes, 1)
	change := batch.Changes[0]
	assert.Equal(t, handlers_dns.ActionUpsert, change.Action)
	assert.Equal(t, testBaseDomain, change.Zone)
	assert.Equal(t, "A", change.Type)

	// The record targets the customer ENI's IP, which survives a VM replace.
	rec := h.record(t, testDBInstanceID)
	assert.Equal(t, rec.ENIPrivateIP, change.Value)
	assert.Equal(t, rec.DNSName, change.Name)
}

// Without northstar there is no name to resolve, so the endpoint has to be the
// ENI IP itself rather than an empty host the client would dial.
func TestCreateDBInstance_EndpointFallsBackToENIIPWithoutBaseDomain(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")

	out, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err)

	rec := h.record(t, testDBInstanceID)
	assert.Empty(t, rec.DNSName)
	assert.NotEmpty(t, rec.ENIPrivateIP)
	assert.Equal(t, rec.ENIPrivateIP, rec.EndpointAddress)
	require.NotNil(t, out.DBInstance.Endpoint)
	assert.Equal(t, rec.ENIPrivateIP, aws.StringValue(out.DBInstance.Endpoint.Address))

	select {
	case batch := <-h.dnsChanges:
		t.Fatalf("no base domain means no record to publish, got %+v", batch)
	default:
	}
}

func TestCreateDBInstance_DuplicateIdentifierIsRejected(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err)

	_, err = h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceAlreadyExists)

	// The loser must not have launched anything: the record reservation is what
	// makes the uniqueness check atomic, so it precedes every resource.
	assert.Empty(t, h.launch.launcher.terminated)
	assert.Equal(t, "i-rds0001", h.record(t, testDBInstanceID).InstanceID)
}

// The reserved record is the identifier's reservation, so a failure after it is
// written must withdraw it — otherwise the name is permanently unusable while
// naming an instance that was never provisioned.
func TestCreateDBInstance_LaunchFailureWithdrawsTheReservation(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	h.launch.launcher.err = errors.New("no host has capacity")

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)
	assert.False(t, h.recordExists(t, testDBInstanceID), "the reserved record must be withdrawn")

	// The same identifier is immediately usable again.
	h.launch.launcher.err = nil
	_, err = h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err)
}

// A failure after the launch returned has to tear down everything the launch
// built. The deferred record delete removes the only thing naming the VM, its
// two ENIs and the data volume, so anything left behind is unreclaimable.
func TestCreateDBInstance_FailureAfterLaunchUnwindsEveryResource(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")

	// Dropping the reserved record while the launch is in flight fails the
	// record-launch step, which is the first thing to run after the resources exist.
	h.launch.launcher.onLaunch = func() {
		kv, err := h.svc.bucket(t.Context(), testAccountID)
		require.NoError(t, err)
		require.NoError(t, kv.Delete(t.Context(), DBInstanceKey(testDBInstanceID)))
	}

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)

	// The VM goes first: the ENIs and volume are in-use while it runs.
	assert.Equal(t, []string{"terminate", "delete-volume", "delete-eni", "delete-eni"}, h.launch.unwind)
	assert.Contains(t, h.launch.launcher.terminated, "i-rds0001")
	assert.Contains(t, h.launch.volumes.deleted, "vol-rdsdata01")
	assert.Len(t, h.launch.enis.deleted, 2, "both the customer and system ENI are deleted")
}

func TestCreateDBInstance_IndexFailureWithdrawsTheRecordedLaunch(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	// This invalid KV key makes the instance-index write fail after recordLaunch
	// has advanced the reservation revision.
	h.launch.launcher.instanceID = "invalid instance id"

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)
	assert.False(t, h.recordExists(t, testDBInstanceID))

	h.launch.launcher.instanceID = ""
	_, err = h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err, "the failed create must release the identifier for reuse")
}

func TestCreateDBInstance_RecordLaunchDoesNotOverwriteAConcurrentReplacement(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	var replacement DBInstanceRecord
	h.launch.launcher.onLaunch = func() {
		replacement = replaceInstanceRecord(t, h.svc, testDBInstanceID)
	}

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)

	assert.Equal(t, replacement, h.record(t, testDBInstanceID))
}

func TestCreateDBInstance_RollbackDoesNotDeleteAConcurrentReplacement(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	h.launch.launcher.instanceID = "invalid instance id"
	var replacement DBInstanceRecord
	h.launch.launcher.onTerminate = func() {
		assert.Equal(t, "invalid instance id", h.record(t, testDBInstanceID).InstanceID,
			"recordLaunch must finish before the replacement race")
		replacement = replaceInstanceRecord(t, h.svc, testDBInstanceID)
	}

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)

	assert.Equal(t, replacement, h.record(t, testDBInstanceID))
}

func TestCreateDBInstance_RollbackFollowsSameOwnerUpdates(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	h.launch.launcher.instanceID = "invalid instance id"
	h.launch.launcher.onTerminate = func() {
		kv, err := h.svc.bucket(t.Context(), testAccountID)
		require.NoError(t, err)
		key := DBInstanceKey(testDBInstanceID)
		var current DBInstanceRecord
		rev, found, err := getJSONRevision(t.Context(), kv, key, &current)
		require.NoError(t, err)
		require.True(t, found)
		current.Tags = map[string]string{"updated": "during-rollback"}
		require.NoError(t, updateJSON(t.Context(), kv, key, rev, &current))
	}

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)

	assert.False(t, h.recordExists(t, testDBInstanceID))
}

// The request may not be answered with an instance whose storage is
// not actually encrypted, so an unencrypted volume fails the whole create.
func TestCreateDBInstance_UnencryptedDataVolumeFailsTheCreate(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	h.launch.volumes.encrypted = false

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)
	assert.False(t, h.recordExists(t, testDBInstanceID))
	assert.Contains(t, h.launch.volumes.deleted, "vol-rdsdata01", "the unencrypted volume is torn down")
}

func TestCreateDBInstance_SecurityGroupsMustBeInThePlacementVPC(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")

	input := validCreateInput()
	input.VpcSecurityGroupIds = aws.StringSlice([]string{"sg-app01"})
	_, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"sg-app01"}, h.record(t, testDBInstanceID).VpcSecurityGroupIDs)

	// An ENI cannot carry a group from another VPC, so this is caught before the
	// launch rather than failing after the record exists.
	input = validCreateInput()
	input.DBInstanceIdentifier = aws.String("other-db")
	input.VpcSecurityGroupIds = aws.StringSlice([]string{"sg-elsewhere"})
	_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.False(t, h.recordExists(t, "other-db"))
}

func TestCreateDBInstance_RequiresADefaultVPCWithASubnet(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	h.network.vpcs = nil

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInvalidVPCNetworkState)

	h.network.vpcs = newFakeNetwork().vpcs
	h.network.subnets = nil
	_, err = h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInvalidVPCNetworkState)

	// A placement failure happens before the reservation, so nothing is left over.
	assert.False(t, h.recordExists(t, testDBInstanceID))
}

// Create validation reads the backup policy off the service, so these cases
// need one. A zero policy carries the built-in defaults.
func newCreateValidator() *Service { return NewService(nil, testRegion) }

// A parameter that would create a false safety, security or availability
// guarantee is rejected rather than silently dropped.
func TestValidateCreateRequest_RejectsUnimplementedParameters(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*rds.CreateDBInstanceInput)
		wantErr string
	}{
		{"MultiAZ", func(in *rds.CreateDBInstanceInput) { in.MultiAZ = aws.Bool(true) }, "MultiAZ"},
		{"PubliclyAccessible", func(in *rds.CreateDBInstanceInput) { in.PubliclyAccessible = aws.Bool(true) }, "PubliclyAccessible"},
		{"StorageEncryptedFalse", func(in *rds.CreateDBInstanceInput) { in.StorageEncrypted = aws.Bool(false) }, "StorageEncrypted"},
		{"IAMDatabaseAuth", func(in *rds.CreateDBInstanceInput) { in.EnableIAMDatabaseAuthentication = aws.Bool(true) }, "EnableIAMDatabaseAuthentication"},
		{"Iops", func(in *rds.CreateDBInstanceInput) { in.Iops = aws.Int64(3000) }, "Iops"},
		{"MaxAllocatedStorage", func(in *rds.CreateDBInstanceInput) { in.MaxAllocatedStorage = aws.Int64(500) }, "MaxAllocatedStorage"},
		{"StorageThroughput", func(in *rds.CreateDBInstanceInput) { in.StorageThroughput = aws.Int64(250) }, "StorageThroughput"},
		{"KmsKeyId", func(in *rds.CreateDBInstanceInput) { in.KmsKeyId = aws.String("arn:aws:kms:::key/abc") }, "KmsKeyId"},
		{"AvailabilityZone", func(in *rds.CreateDBInstanceInput) { in.AvailabilityZone = aws.String("ap-southeast-2b") }, "AvailabilityZone"},
		{"DBSecurityGroups", func(in *rds.CreateDBInstanceInput) { in.DBSecurityGroups = aws.StringSlice([]string{"classic"}) }, "DBSecurityGroups"},
		{"DBClusterIdentifier", func(in *rds.CreateDBInstanceInput) { in.DBClusterIdentifier = aws.String("cluster-1") }, "DBClusterIdentifier"},
		{"CloudwatchLogsExports", func(in *rds.CreateDBInstanceInput) {
			in.EnableCloudwatchLogsExports = aws.StringSlice([]string{"postgresql"})
		}, "EnableCloudwatchLogsExports"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := validCreateInput()
			tc.mutate(input)
			_, err := newCreateValidator().validateCreateRequest(input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// DeletionProtection is honoured, so it has to be accepted
// at create and carried onto the record DeleteDBInstance checks.
func TestValidateCreateRequest_AcceptsDeletionProtection(t *testing.T) {
	t.Parallel()
	input := validCreateInput()
	input.DeletionProtection = aws.Bool(true)

	req, err := newCreateValidator().validateCreateRequest(input)
	require.NoError(t, err)
	assert.True(t, req.DeletionProtection)
}

func TestValidateCreateRequest_RejectsMalformedRequests(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*rds.CreateDBInstanceInput)
		wantError string
	}{
		{"NoIdentifier", func(in *rds.CreateDBInstanceInput) { in.DBInstanceIdentifier = nil }, awserrors.ErrorInvalidParameterValue},
		{"IdentifierUppercase", func(in *rds.CreateDBInstanceInput) { in.DBInstanceIdentifier = aws.String("OrdersDB") }, awserrors.ErrorInvalidParameterValue},
		{"IdentifierLeadingDigit", func(in *rds.CreateDBInstanceInput) { in.DBInstanceIdentifier = aws.String("1db") }, awserrors.ErrorInvalidParameterValue},
		{"IdentifierTrailingHyphen", func(in *rds.CreateDBInstanceInput) { in.DBInstanceIdentifier = aws.String("db-") }, awserrors.ErrorInvalidParameterValue},
		{"IdentifierDoubleHyphen", func(in *rds.CreateDBInstanceInput) { in.DBInstanceIdentifier = aws.String("or--ders") }, awserrors.ErrorInvalidParameterValue},
		{"IdentifierTooLong", func(in *rds.CreateDBInstanceInput) {
			in.DBInstanceIdentifier = aws.String(strings.Repeat("a", maxDBInstanceIdentifierLen+1))
		}, awserrors.ErrorInvalidParameterValue},
		{"UnknownEngine", func(in *rds.CreateDBInstanceInput) { in.Engine = aws.String("mysql") }, awserrors.ErrorInvalidParameterValue},
		{"MinorEngineVersion", func(in *rds.CreateDBInstanceInput) { in.EngineVersion = aws.String("18.4") }, awserrors.ErrorInvalidParameterValue},
		{"UnknownClass", func(in *rds.CreateDBInstanceInput) { in.DBInstanceClass = aws.String("db.x99.mega") }, awserrors.ErrorInvalidParameterValue},
		{"StorageBelowMinimum", func(in *rds.CreateDBInstanceInput) {
			in.AllocatedStorage = aws.Int64(minAllocatedStorageGiB - 1)
		}, awserrors.ErrorInvalidParameterValue},
		{"StorageAboveMaximum", func(in *rds.CreateDBInstanceInput) {
			in.AllocatedStorage = aws.Int64(maxAllocatedStorageGiB + 1)
		}, awserrors.ErrorInvalidParameterValue},
		{"StorageType", func(in *rds.CreateDBInstanceInput) { in.StorageType = aws.String("io2") }, awserrors.ErrorInvalidParameterValue},
		{"PortTooLow", func(in *rds.CreateDBInstanceInput) { in.Port = aws.Int64(80) }, awserrors.ErrorInvalidParameterValue},
		{"ReservedUsername", func(in *rds.CreateDBInstanceInput) { in.MasterUsername = aws.String("rdsadmin") }, awserrors.ErrorInvalidParameterValue},
		{"ShortPassword", func(in *rds.CreateDBInstanceInput) { in.MasterUserPassword = aws.String("short") }, awserrors.ErrorInvalidParameterValue},
		{"DBNameHyphen", func(in *rds.CreateDBInstanceInput) { in.DBName = aws.String("my-db") }, awserrors.ErrorInvalidParameterValue},
		{"DBNameTooLong", func(in *rds.CreateDBInstanceInput) {
			in.DBName = aws.String(strings.Repeat("a", 64))
		}, awserrors.ErrorInvalidParameterValue},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := validCreateInput()
			tc.mutate(input)
			_, err := newCreateValidator().validateCreateRequest(input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestValidateCreateRequest_AcceptsTheImplicitDefaultParameterGroup(t *testing.T) {
	t.Parallel()
	input := validCreateInput()
	input.DBParameterGroupName = aws.String("default.postgres18")
	input.Port = aws.Int64(6543)

	req, err := newCreateValidator().validateCreateRequest(input)
	require.NoError(t, err)
	assert.Equal(t, "default.postgres18", req.DBParameterGroupName)
	assert.Equal(t, int64(6543), req.Port)
	assert.Equal(t, storageTypeGP3, req.StorageType, "gp3 is the default when the request names no storage type")
	assert.Equal(t, "t3.medium", req.InstanceType)
}

// The inert parameters are accepted as no-ops rather than rejected: omitting
// them creates no false guarantee, so a Terraform config carrying them works.
func TestValidateCreateRequest_AcceptsInertParameters(t *testing.T) {
	t.Parallel()
	input := validCreateInput()
	input.AutoMinorVersionUpgrade = aws.Bool(true)
	input.CopyTagsToSnapshot = aws.Bool(true)
	input.EnablePerformanceInsights = aws.Bool(true)
	input.MonitoringInterval = aws.Int64(60)
	input.StorageEncrypted = aws.Bool(true)

	req, err := newCreateValidator().validateCreateRequest(input)
	require.NoError(t, err)
	assert.True(t, req.AutoMinorVersionUpgrade)
	assert.True(t, req.CopyTagsToSnapshot)
	assert.True(t, req.EnablePerformanceInsights)
	assert.Equal(t, int64(60), req.MonitoringInterval)
}
