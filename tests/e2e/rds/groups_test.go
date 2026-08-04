//go:build e2e

package rds

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The engine version the control plane pins, so the family a customer names
	// has to match what CreateDBInstance would resolve on its own.
	dbParameterGroupFamily = "postgres18"
	// A dynamic parameter, so the engine adopts it on a reload and the assertion
	// does not have to wait out a restart. Its default is 4096 kB.
	workMemKiB = "8192"
)

// TestSubnetAndParameterGroups drives the customer-managed group path end to
// end: a subnet group the instance is placed from, a parameter group whose
// override the running engine actually reports, and the two deletes that must be
// refused while the instance holds them and accepted once it is gone.
func TestSubnetAndParameterGroups(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	reserveDBVMs(t, dbClass)

	suffix := time.Now().Unix()
	subnetGroup := fmt.Sprintf("%s-subnets-%d", dbInstancePfx, suffix)
	paramGroup := fmt.Sprintf("%s-params-%d", dbInstancePfx, suffix)
	id := fmt.Sprintf("%s-groups-%d", dbInstancePfx, suffix)

	vpcID, _, _ := harness.DiscoverDefaultVPC(t, f.AWS)
	subnets := subnetsInVPC(t, f.AWS, vpcID)
	require.NotEmpty(t, subnets, "the default VPC %s has no subnets to build a DB subnet group from", vpcID)

	harness.Phase(t, "Creating DB subnet group %q over %d subnet(s) in %s", subnetGroup, len(subnets), vpcID)
	subnetIDs := make([]string, 0, len(subnets))
	for _, s := range subnets {
		subnetIDs = append(subnetIDs, aws.StringValue(s.SubnetId))
	}
	createdSubnets, err := f.AWS.RDS.CreateDBSubnetGroup(&rds.CreateDBSubnetGroupInput{
		DBSubnetGroupName:        aws.String(subnetGroup),
		DBSubnetGroupDescription: aws.String("rds e2e subnet group"),
		SubnetIds:                aws.StringSlice(subnetIDs),
	})
	require.NoError(t, err, "create-db-subnet-group")
	require.NotNil(t, createdSubnets.DBSubnetGroup)
	assert.Equal(t, vpcID, aws.StringValue(createdSubnets.DBSubnetGroup.VpcId))
	assert.Equal(t, "Complete", aws.StringValue(createdSubnets.DBSubnetGroup.SubnetGroupStatus))
	assert.ElementsMatch(t, subnetIDs, groupSubnetIDs(createdSubnets.DBSubnetGroup),
		"every subnet supplied must be stored on the group")
	for _, s := range createdSubnets.DBSubnetGroup.Subnets {
		require.NotNil(t, s.SubnetAvailabilityZone)
		assert.NotEmpty(t, aws.StringValue(s.SubnetAvailabilityZone.Name),
			"subnet %s must report the zone it is actually in", aws.StringValue(s.SubnetIdentifier))
	}

	harness.Phase(t, "Creating DB parameter group %q", paramGroup)
	_, err = f.AWS.RDS.CreateDBParameterGroup(&rds.CreateDBParameterGroupInput{
		DBParameterGroupName:   aws.String(paramGroup),
		DBParameterGroupFamily: aws.String(dbParameterGroupFamily),
		Description:            aws.String("rds e2e parameter group"),
	})
	require.NoError(t, err, "create-db-parameter-group")

	_, err = f.AWS.RDS.ModifyDBParameterGroup(&rds.ModifyDBParameterGroupInput{
		DBParameterGroupName: aws.String(paramGroup),
		Parameters: []*rds.Parameter{{
			ParameterName:  aws.String("work_mem"),
			ParameterValue: aws.String(workMemKiB),
			ApplyMethod:    aws.String("immediate"),
		}},
	})
	require.NoError(t, err, "modify-db-parameter-group")

	// Registered before the create, so LIFO runs it last: the create registers its
	// own instance teardown and, after it, the failure-only diagnostics. A group
	// teardown registered later would run first and delete the DB VM out from
	// under the console capture, which is the only window into the guest.
	t.Cleanup(func() {
		teardownGroups(t, f, subnetGroup, paramGroup)
	})

	// The size-derived defaults are formulas internally, but a customer only
	// ever sees the literal they resolved to.
	t.Run("DescribeDBParametersReportsLiterals", func(t *testing.T) {
		params := describeAllParameters(t, f, paramGroup)

		workMem, ok := params["work_mem"]
		require.True(t, ok, "the modified parameter must be reported")
		assert.Equal(t, workMemKiB, aws.StringValue(workMem.ParameterValue))
		assert.Equal(t, "user", aws.StringValue(workMem.Source))

		shared, ok := params["shared_buffers"]
		require.True(t, ok, "a parameter the group does not set must still be reported")
		assert.Equal(t, "engine-default", aws.StringValue(shared.Source))
		assert.NotContains(t, aws.StringValue(shared.ParameterValue), "{",
			"a size-derived default must reach the customer as a literal")
		assert.Equal(t, "static", aws.StringValue(shared.ApplyType))
	})

	// The value the engine would refuse has to be refused here, not handed to a
	// cluster that then fails to start.
	t.Run("RejectsAValueTheEngineWouldRefuse", func(t *testing.T) {
		harness.ExpectError(t, "InvalidParameterValue", func() error {
			_, err := f.AWS.RDS.ModifyDBParameterGroup(&rds.ModifyDBParameterGroupInput{
				DBParameterGroupName: aws.String(paramGroup),
				Parameters: []*rds.Parameter{{
					ParameterName:  aws.String("max_connections"),
					ParameterValue: aws.String("{DBInstanceClassMemory/9531392}"),
				}},
			})
			return err
		})
	})

	harness.Phase(t, "Creating DB instance %q against both groups", id)
	created := createDBInstance(t, f, id, func(in *rds.CreateDBInstanceInput) {
		in.DBSubnetGroupName = aws.String(subnetGroup)
		in.DBParameterGroupName = aws.String(paramGroup)
	})

	assert.Equal(t, subnetGroup, dbSubnetGroupName(created))
	assert.Equal(t, paramGroup, dbParameterGroupName(created))

	var instance *rds.DBInstance
	t.Run("BecomesAvailableOnTheResolvedParameters", func(t *testing.T) {
		instance = waitForAvailable(t, f, id)
		assert.Equal(t, subnetGroup, dbSubnetGroupName(instance),
			"the describe must report the group the instance was placed from")
		assert.Equal(t, paramGroup, dbParameterGroupName(instance))
	})

	// The only assertion that proves the resolved set reached the engine rather
	// than merely being stored: the running cluster reports the override. Asked
	// from the client VM, since the endpoint is reachable from nowhere else.
	t.Run("TheEngineRunsTheOverride", func(t *testing.T) {
		requireAvailable(t, instance)
		client := rdsClient(t, f)
		conn := harness.PSQLConnFor(t, instance, dbMasterUser, dbMasterPassword, dbName)
		out := harness.PSQL(t, client, conn, "SHOW work_mem;")
		assert.Equal(t, "8MB", strings.TrimSpace(out), "the engine is not running the group's work_mem")
	})

	t.Run("GroupsInUseCannotBeDeleted", func(t *testing.T) {
		requireAvailable(t, instance)
		harness.ExpectError(t, "InvalidDBSubnetGroupStateFault", func() error {
			_, err := f.AWS.RDS.DeleteDBSubnetGroup(&rds.DeleteDBSubnetGroupInput{
				DBSubnetGroupName: aws.String(subnetGroup),
			})
			return err
		})
		harness.ExpectError(t, "InvalidDBParameterGroupState", func() error {
			_, err := f.AWS.RDS.DeleteDBParameterGroup(&rds.DeleteDBParameterGroupInput{
				DBParameterGroupName: aws.String(paramGroup),
			})
			return err
		})
	})

	t.Run("GroupsFreeUpOnceTheInstanceIsGone", func(t *testing.T) {
		requireAvailable(t, instance)
		harness.Phase(t, "Deleting DB instance %q to free its groups", id)
		_, err := f.AWS.RDS.DeleteDBInstance(&rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: aws.String(id),
			SkipFinalSnapshot:    aws.Bool(true),
		})
		require.NoError(t, err, "delete-db-instance")
		harness.WaitForDBInstanceGone(t, f.AWS, id)

		_, err = f.AWS.RDS.DeleteDBSubnetGroup(&rds.DeleteDBSubnetGroupInput{
			DBSubnetGroupName: aws.String(subnetGroup),
		})
		require.NoError(t, err, "delete-db-subnet-group after the instance is gone")

		_, err = f.AWS.RDS.DeleteDBParameterGroup(&rds.DeleteDBParameterGroupInput{
			DBParameterGroupName: aws.String(paramGroup),
		})
		require.NoError(t, err, "delete-db-parameter-group after the instance is gone")

		harness.ExpectError(t, "DBSubnetGroupNotFoundFault", func() error {
			_, err := f.AWS.RDS.DescribeDBSubnetGroups(&rds.DescribeDBSubnetGroupsInput{
				DBSubnetGroupName: aws.String(subnetGroup),
			})
			return err
		})
	})

	// default.postgres18 is implicit rather than stored, so it has to be
	// describable and undeletable without ever having been created.
	t.Run("TheDefaultParameterGroupIsImplicitAndUndeletable", func(t *testing.T) {
		out, err := f.AWS.RDS.DescribeDBParameterGroups(&rds.DescribeDBParameterGroupsInput{
			DBParameterGroupName: aws.String("default." + dbParameterGroupFamily),
		})
		require.NoError(t, err, "describe-db-parameter-groups")
		require.Len(t, out.DBParameterGroups, 1)
		assert.Equal(t, dbParameterGroupFamily, aws.StringValue(out.DBParameterGroups[0].DBParameterGroupFamily))

		harness.ExpectError(t, "InvalidDBParameterGroupState", func() error {
			_, err := f.AWS.RDS.DeleteDBParameterGroup(&rds.DeleteDBParameterGroupInput{
				DBParameterGroupName: aws.String("default." + dbParameterGroupFamily),
			})
			return err
		})
	})
}

// Best-effort teardown: the groups can only go once the instance does, and every
// step is already the assertion of some subtest, so a failure here is logged
// rather than failing a test that otherwise passed.
// Frees the two groups only. The instance that referenced them is torn down by
// the create's own cleanup, which is registered later and so runs first — a
// group delete issued while the instance still holds it is refused.
func teardownGroups(t *testing.T, f *Fixture, subnetGroup, paramGroup string) {
	t.Helper()
	if _, err := f.AWS.RDS.DeleteDBSubnetGroup(&rds.DeleteDBSubnetGroupInput{
		DBSubnetGroupName: aws.String(subnetGroup),
	}); err != nil && !harness.ErrorCodeIs(err, "DBSubnetGroupNotFoundFault") {
		t.Logf("cleanup: delete DB subnet group %s: %v", subnetGroup, err)
	}
	if _, err := f.AWS.RDS.DeleteDBParameterGroup(&rds.DeleteDBParameterGroupInput{
		DBParameterGroupName: aws.String(paramGroup),
	}); err != nil && !harness.ErrorCodeIs(err, "DBParameterGroupNotFound") {
		t.Logf("cleanup: delete DB parameter group %s: %v", paramGroup, err)
	}
}

// The catalog is larger than one page, so a describe that stopped at the first
// response would miss whatever sorts late.
func describeAllParameters(t *testing.T, f *Fixture, group string) map[string]*rds.Parameter {
	t.Helper()
	params := map[string]*rds.Parameter{}
	var marker *string
	for {
		out, err := f.AWS.RDS.DescribeDBParameters(&rds.DescribeDBParametersInput{
			DBParameterGroupName: aws.String(group),
			Marker:               marker,
		})
		require.NoError(t, err, "describe-db-parameters")
		for _, p := range out.Parameters {
			params[aws.StringValue(p.ParameterName)] = p
		}
		if marker = out.Marker; aws.StringValue(marker) == "" {
			return params
		}
	}
}

func subnetsInVPC(t *testing.T, c *harness.AWSClient, vpcID string) []*ec2.Subnet {
	t.Helper()
	out, err := c.EC2.DescribeSubnets(&ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}}},
	})
	require.NoError(t, err, "describe-subnets")
	return out.Subnets
}

func groupSubnetIDs(group *rds.DBSubnetGroup) []string {
	ids := make([]string, 0, len(group.Subnets))
	for _, s := range group.Subnets {
		ids = append(ids, aws.StringValue(s.SubnetIdentifier))
	}
	return ids
}

func dbSubnetGroupName(instance *rds.DBInstance) string {
	if instance.DBSubnetGroup == nil {
		return ""
	}
	return aws.StringValue(instance.DBSubnetGroup.DBSubnetGroupName)
}

func dbParameterGroupName(instance *rds.DBInstance) string {
	if len(instance.DBParameterGroups) == 0 {
		return ""
	}
	return aws.StringValue(instance.DBParameterGroups[0].DBParameterGroupName)
}
