//go:build e2e

package rds

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateDescribe drives the control-plane half of the first live RDS path:
// CreateDBInstance (postgres, single-AZ) → available → the describe and tag
// views a customer and the Terraform provider read it back through.
//
// The client leg is TestConnectivity's: a connection is only meaningful from
// inside the customer VPC. The instance is created once and shared by the
// subtests, because booting the VM and running initdb is by far the slowest step.
func TestCreateDescribe(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	reserveDBVMs(t, 1)

	id := fmt.Sprintf("%s-%d", dbInstancePfx, time.Now().Unix())

	harness.Phase(t, "Creating DB instance %q", id)
	created := createDBInstance(t, f, id, func(in *rds.CreateDBInstanceInput) {
		in.Tags = []*rds.Tag{
			{Key: aws.String("env"), Value: aws.String("e2e")},
			{Key: aws.String("suite"), Value: aws.String("rds")},
		}
	})

	// The create returns before the engine exists, so the customer sees creating
	// and polls; the reconciler owns the flip to available.
	assert.Equal(t, harness.DBInstanceCreating, aws.StringValue(created.DBInstanceStatus))
	assert.Equal(t, id, aws.StringValue(created.DBInstanceIdentifier))

	var instance *rds.DBInstance
	t.Run("BecomesAvailable", func(t *testing.T) {
		instance = waitForAvailable(t, f, id)

		assert.Equal(t, dbEngine, aws.StringValue(instance.Engine))
		assert.Equal(t, dbClass, aws.StringValue(instance.DBInstanceClass))
		assert.Equal(t, int64(dbStorageGiB), aws.Int64Value(instance.AllocatedStorage))
		assert.Equal(t, dbMasterUser, aws.StringValue(instance.MasterUsername))
		assert.True(t, aws.BoolValue(instance.StorageEncrypted), "the data volume is encrypted with the cluster key")
		assert.False(t, aws.BoolValue(instance.MultiAZ))
		assert.False(t, aws.BoolValue(instance.PubliclyAccessible))

		require.NotNil(t, instance.Endpoint, "an available instance must publish an endpoint")
		assert.NotEmpty(t, aws.StringValue(instance.Endpoint.Address))
		assert.Equal(t, int64(5432), aws.Int64Value(instance.Endpoint.Port))
	})

	t.Run("AppearsInTheFleetListing", func(t *testing.T) {
		requireAvailable(t, instance)
		list, err := f.AWS.RDS.DescribeDBInstances(&rds.DescribeDBInstancesInput{})
		require.NoError(t, err, "describe-db-instances")

		ids := make([]string, 0, len(list.DBInstances))
		for _, i := range list.DBInstances {
			ids = append(ids, aws.StringValue(i.DBInstanceIdentifier))
		}
		assert.Contains(t, ids, id, "an unfiltered describe must list the instance")
	})

	// The Terraform provider reads tags through ListTagsForResource on every
	// apply, so this is the assertion that proves the provider's read path works.
	t.Run("TagsRoundTrip", func(t *testing.T) {
		requireAvailable(t, instance)
		arn := aws.StringValue(instance.DBInstanceArn)
		require.NotEmpty(t, arn, "an available instance must publish its ARN")

		tags, err := f.AWS.RDS.ListTagsForResource(&rds.ListTagsForResourceInput{ResourceName: aws.String(arn)})
		require.NoError(t, err, "list-tags-for-resource")
		assert.Equal(t, map[string]string{"env": "e2e", "suite": "rds"}, tagMap(tags.TagList),
			"the tags supplied at create must be readable back")

		_, err = f.AWS.RDS.AddTagsToResource(&rds.AddTagsToResourceInput{
			ResourceName: aws.String(arn),
			Tags:         []*rds.Tag{{Key: aws.String("env"), Value: aws.String("e2e-updated")}},
		})
		require.NoError(t, err, "add-tags-to-resource")

		_, err = f.AWS.RDS.RemoveTagsFromResource(&rds.RemoveTagsFromResourceInput{
			ResourceName: aws.String(arn),
			TagKeys:      aws.StringSlice([]string{"suite", "never-set"}),
		})
		require.NoError(t, err, "remove-tags-from-resource must ignore a key that is not present")

		tags, err = f.AWS.RDS.ListTagsForResource(&rds.ListTagsForResourceInput{ResourceName: aws.String(arn)})
		require.NoError(t, err, "list-tags-for-resource")
		assert.Equal(t, map[string]string{"env": "e2e-updated"}, tagMap(tags.TagList))

		// Terraform reads tags from the describe as well, so the two views
		// disagreeing would show up as permanent drift.
		described, err := f.AWS.RDS.DescribeDBInstances(&rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(id),
		})
		require.NoError(t, err, "describe-db-instances")
		require.Len(t, described.DBInstances, 1)
		assert.Equal(t, map[string]string{"env": "e2e-updated"}, tagMap(described.DBInstances[0].TagList))
	})

	// The endpoint's name, its resolution inside the guest and the client
	// connection itself belong to TestConnectivity: a lookup or a psql on the
	// machine running the test proves nothing about the path a customer takes.
	t.Run("PublishesAnEndpoint", func(t *testing.T) {
		requireAvailable(t, instance)
		host := aws.StringValue(instance.Endpoint.Address)
		if f.BaseDomain == "" {
			assert.NotNil(t, net.ParseIP(host),
				"with no base domain the endpoint is the bare ENI address, got %s", host)
			return
		}
		assert.Equal(t, fmt.Sprintf("%s.%s.%s.rds.%s", id, f.Account, f.Region, f.BaseDomain), host,
			"the endpoint name is account-qualified so identifiers collide across tenants without colliding in DNS")
	})
}

func tagMap(tags []*rds.Tag) map[string]string {
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		out[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return out
}

func requireAvailable(t *testing.T, instance *rds.DBInstance) {
	t.Helper()
	if instance == nil {
		t.Skip("DB instance never reached available (BecomesAvailable failed)")
	}
}
