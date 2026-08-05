package handlers_rds

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSubnetGroup = "db-private"

func subnetGroupInput(name string, subnetIDs ...string) *rds.CreateDBSubnetGroupInput {
	return &rds.CreateDBSubnetGroupInput{
		DBSubnetGroupName:        aws.String(name),
		DBSubnetGroupDescription: aws.String("Database subnets"),
		SubnetIds:                aws.StringSlice(subnetIDs),
	}
}

// Single-AZ groups are valid here: AWS's two-AZ rule would fail every stock
// Terraform module against a platform that exposes one zone, and rejecting it
// buys no safety.
func TestCreateDBSubnetGroup_AcceptsASingleSubnet(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)

	out, err := h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput(testSubnetGroup, "subnet-alpha"), testAccountID)
	require.NoError(t, err)

	group := out.DBSubnetGroup
	require.NotNil(t, group)
	assert.Equal(t, testSubnetGroup, aws.StringValue(group.DBSubnetGroupName))
	assert.Equal(t, DBSubnetGroupARN(testRegion, testAccountID, testSubnetGroup),
		aws.StringValue(group.DBSubnetGroupArn))
	assert.Equal(t, testDefaultVPC, aws.StringValue(group.VpcId))
	require.Len(t, group.Subnets, 1)
	assert.Equal(t, "subnet-alpha", aws.StringValue(group.Subnets[0].SubnetIdentifier))
	// Reported from the subnet's own record rather than a hardcoded zone, so the
	// response shape survives V2 making AZs real.
	require.NotNil(t, group.Subnets[0].SubnetAvailabilityZone)
	assert.Equal(t, testZone, aws.StringValue(group.Subnets[0].SubnetAvailabilityZone.Name))
}

// Stored verbatim: the group keeps every subnet the customer supplied, so the
// placement logic can change later without a record migration.
func TestCreateDBSubnetGroup_StoresEverySubnetSupplied(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.CreateDBSubnetGroup(t.Context(),
		subnetGroupInput(testSubnetGroup, "subnet-zebra", "subnet-alpha"), testAccountID)
	require.NoError(t, err)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	rec, _, err := getDBSubnetGroup(t.Context(), kv, testSubnetGroup)
	require.NoError(t, err)

	require.Len(t, rec.Subnets, 2)
	assert.Equal(t, "subnet-zebra", rec.Subnets[0].SubnetID, "the request's own order is preserved")
	assert.Equal(t, "subnet-alpha", rec.Subnets[1].SubnetID)
	assert.Equal(t, testDefaultVPC, rec.VpcID)
}

func TestCreateDBSubnetGroup_RejectsAMissingSubnet(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.CreateDBSubnetGroup(t.Context(),
		subnetGroupInput(testSubnetGroup, "subnet-alpha", "subnet-nowhere"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBSubnetInvalid, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.Contains(t, err.Error(), "subnet-nowhere")
}

// The check that actually matters: a group spanning two VPCs cannot host an
// instance at all, because the endpoint ENI lives in exactly one of them.
func TestCreateDBSubnetGroup_RejectsACrossVPCSet(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	h.network.subnets = append(h.network.subnets, &ec2.Subnet{
		SubnetId:         aws.String("subnet-other"),
		VpcId:            aws.String("vpc-other01"),
		AvailabilityZone: aws.String(testZone),
	})

	_, err := h.svc.CreateDBSubnetGroup(t.Context(),
		subnetGroupInput(testSubnetGroup, "subnet-alpha", "subnet-other"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBSubnetInvalid, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.Contains(t, err.Error(), "must span one VPC")
}

// The describe is issued as the caller, so another account's subnet does not
// come back at all — it is reported as invalid rather than leaked or accepted.
func TestCreateDBSubnetGroup_RejectsACrossAccountSubnet(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.CreateDBSubnetGroup(t.Context(),
		subnetGroupInput(testSubnetGroup, "subnet-foreign"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBSubnetInvalid, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.Equal(t, []string{testAccountID}, h.network.accts,
		"the subnet describe must be issued against the caller's own account")
}

func TestCreateDBSubnetGroup_RejectsMalformedRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rds.CreateDBSubnetGroupInput)
		want   string
	}{
		{"NoName", func(in *rds.CreateDBSubnetGroupInput) { in.DBSubnetGroupName = nil }, awserrors.ErrorInvalidParameterValue},
		{"ReservedName", func(in *rds.CreateDBSubnetGroupInput) { in.DBSubnetGroupName = aws.String("default") }, awserrors.ErrorInvalidParameterValue},
		{"LeadingDigit", func(in *rds.CreateDBSubnetGroupInput) { in.DBSubnetGroupName = aws.String("1group") }, awserrors.ErrorInvalidParameterValue},
		{"NameWithSlash", func(in *rds.CreateDBSubnetGroupInput) { in.DBSubnetGroupName = aws.String("db/private") }, awserrors.ErrorInvalidParameterValue},
		{"NameWithPlus", func(in *rds.CreateDBSubnetGroupInput) { in.DBSubnetGroupName = aws.String("db+private") }, awserrors.ErrorInvalidParameterValue},
		{"NameWithParentheses", func(in *rds.CreateDBSubnetGroupInput) { in.DBSubnetGroupName = aws.String("db(1)") }, awserrors.ErrorInvalidParameterValue},
		{"NoDescription", func(in *rds.CreateDBSubnetGroupInput) { in.DBSubnetGroupDescription = nil }, awserrors.ErrorInvalidParameterValue},
		{"NoSubnets", func(in *rds.CreateDBSubnetGroupInput) { in.SubnetIds = nil }, awserrors.ErrorInvalidParameterValue},
		{"DuplicateSubnet", func(in *rds.CreateDBSubnetGroupInput) {
			in.SubnetIds = aws.StringSlice([]string{"subnet-alpha", "subnet-alpha"})
		}, awserrors.ErrorDBSubnetInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCreateHarness(t, testBaseDomain)
			input := subnetGroupInput(testSubnetGroup, "subnet-alpha")
			tc.mutate(input)

			_, err := h.svc.CreateDBSubnetGroup(t.Context(), input, testAccountID)
			require.Error(t, err)
			assert.Equal(t, tc.want, awserrors.ValidErrorCodeFromError(err),
				"the code has to survive resolution or the client sees a 500")
		})
	}
}

// The name is the reservation, so a repeat is a conflict rather than a silent
// overwrite of the first group's subnets.
func TestCreateDBSubnetGroup_RejectsADuplicateName(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput(testSubnetGroup, "subnet-alpha"), testAccountID)
	require.NoError(t, err)

	_, err = h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput(testSubnetGroup, "subnet-zebra"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBSubnetGroupAlreadyExists, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

func TestDescribeDBSubnetGroups_ListsAndNames(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput("zeta", "subnet-alpha"), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput("alpha", "subnet-zebra"), testAccountID)
	require.NoError(t, err)

	listed, err := h.svc.DescribeDBSubnetGroups(t.Context(), &rds.DescribeDBSubnetGroupsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.DBSubnetGroups, 2)
	assert.Equal(t, "alpha", aws.StringValue(listed.DBSubnetGroups[0].DBSubnetGroupName),
		"groups are sorted, so a repeated list does not read as drift")

	named, err := h.svc.DescribeDBSubnetGroups(t.Context(),
		&rds.DescribeDBSubnetGroupsInput{DBSubnetGroupName: aws.String("zeta")}, testAccountID)
	require.NoError(t, err)
	require.Len(t, named.DBSubnetGroups, 1)
	assert.Equal(t, "zeta", aws.StringValue(named.DBSubnetGroups[0].DBSubnetGroupName))
}

// A client polling a create would read an empty list as "gone" rather than
// "not there", so a named group that does not exist is an error.
func TestDescribeDBSubnetGroups_RejectsAnUnknownName(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)

	_, err := h.svc.DescribeDBSubnetGroups(t.Context(),
		&rds.DescribeDBSubnetGroupsInput{DBSubnetGroupName: aws.String("absent")}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBSubnetGroupNotFound, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

func TestDeleteDBSubnetGroup_RemovesAnUnusedGroup(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput(testSubnetGroup, "subnet-alpha"), testAccountID)
	require.NoError(t, err)

	_, err = h.svc.DeleteDBSubnetGroup(t.Context(),
		&rds.DeleteDBSubnetGroupInput{DBSubnetGroupName: aws.String(testSubnetGroup)}, testAccountID)
	require.NoError(t, err)

	_, err = h.svc.DescribeDBSubnetGroups(t.Context(),
		&rds.DescribeDBSubnetGroupsInput{DBSubnetGroupName: aws.String(testSubnetGroup)}, testAccountID)
	assert.Equal(t, awserrors.ErrorDBSubnetGroupNotFound, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// A group referenced only by a deleting instance is refused too, so teardown
// ordering stays unambiguous rather than racing the instance's own delete.
func TestDeleteDBSubnetGroup_RefusesWhileAnInstanceReferencesIt(t *testing.T) {
	for _, status := range []Status{StatusAvailable, StatusDeleting} {
		t.Run(string(status), func(t *testing.T) {
			h := newCreateHarness(t, testBaseDomain)
			_, err := h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput(testSubnetGroup, "subnet-alpha"), testAccountID)
			require.NoError(t, err)

			input := validCreateInput()
			input.DBSubnetGroupName = aws.String(testSubnetGroup)
			_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
			require.NoError(t, err)

			kv, err := h.svc.bucket(t.Context(), testAccountID)
			require.NoError(t, err)
			require.NoError(t, h.svc.updateInstance(t.Context(), kv, testDBInstanceID, func(rec *DBInstanceRecord) {
				rec.Status = status
			}))

			_, err = h.svc.DeleteDBSubnetGroup(t.Context(),
				&rds.DeleteDBSubnetGroupInput{DBSubnetGroupName: aws.String(testSubnetGroup)}, testAccountID)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorDBSubnetGroupInvalidState, awserrors.ValidErrorCodeFromError(err),
				"the code has to survive resolution or the client sees a 500")
			assert.Contains(t, err.Error(), testDBInstanceID)
		})
	}
}

// The group is what decides where the endpoint lands, so a create naming one has
// to place the ENI in its subnet rather than in the account's default VPC.
func TestCreateDBInstance_PlacesTheEndpointFromTheNamedSubnetGroup(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput(testSubnetGroup, "subnet-zebra"), testAccountID)
	require.NoError(t, err)

	input := validCreateInput()
	input.DBSubnetGroupName = aws.String(testSubnetGroup)
	_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)

	rec := h.record(t, testDBInstanceID)
	assert.Equal(t, "subnet-zebra", rec.SubnetID,
		"the group's own subnet is the placement, not the default VPC's first")
	assert.Equal(t, testSubnetGroup, rec.DBSubnetGroupName)
	assert.Equal(t, testDefaultVPC, rec.VpcID)
}

// Asserted through the describe rather than the record: the group the instance
// was placed from is only useful to a client if it survives the projection, and
// the Terraform provider reads db_subnet_group_name from exactly here.
func TestDescribeDBInstances_ReportsTheSubnetGroupTheInstanceWasPlacedFrom(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBSubnetGroup(t.Context(), subnetGroupInput(testSubnetGroup, "subnet-zebra"), testAccountID)
	require.NoError(t, err)

	input := validCreateInput()
	input.DBSubnetGroupName = aws.String(testSubnetGroup)
	created, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, created.DBInstance)
	require.NotNil(t, created.DBInstance.DBSubnetGroup)
	assert.Equal(t, testSubnetGroup, aws.StringValue(created.DBInstance.DBSubnetGroup.DBSubnetGroupName))

	out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBInstances, 1)

	group := out.DBInstances[0].DBSubnetGroup
	require.NotNil(t, group)
	assert.Equal(t, testSubnetGroup, aws.StringValue(group.DBSubnetGroupName))
	assert.Equal(t, testDefaultVPC, aws.StringValue(group.VpcId))
	assert.Equal(t, subnetGroupStatusComplete, aws.StringValue(group.SubnetGroupStatus))
}

// Placing the endpoint somewhere else would put it in a subnet nobody chose, so
// a group that does not exist fails the create rather than falling back.
func TestCreateDBInstance_RejectsAnUnknownSubnetGroup(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)

	input := validCreateInput()
	input.DBSubnetGroupName = aws.String("absent")
	_, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDBSubnetGroupNotFound, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.False(t, h.recordExists(t, testDBInstanceID),
		"a rejected create must reserve nothing")
}
