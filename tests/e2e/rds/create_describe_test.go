//go:build e2e

package rds

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateDescribeConnect drives the first live RDS path: CreateDBInstance
// (postgres, single-AZ) → available → an endpoint that resolves → a client that
// connects with the master credentials and writes a row.
//
// The instance is created once and shared by the subtests: booting the VM and
// running initdb is by far the slowest step.
func TestCreateDescribeConnect(t *testing.T) {
	f := requireRDSFixture(t)
	id := fmt.Sprintf("%s-%d", dbInstancePfx, time.Now().Unix())

	harness.Phase(t, "Creating DB instance %q", id)
	out, err := f.AWS.RDS.CreateDBInstance(&rds.CreateDBInstanceInput{ //nolint:staticcheck // e2e:allow-create — the instance under test
		DBInstanceIdentifier: aws.String(id),
		Engine:               aws.String(dbEngine),
		DBInstanceClass:      aws.String(dbClass),
		AllocatedStorage:     aws.Int64(dbStorageGiB),
		DBName:               aws.String(dbName),
		MasterUsername:       aws.String(dbMasterUser),
		MasterUserPassword:   aws.String(dbMasterPassword),
		Tags: []*rds.Tag{
			{Key: aws.String("env"), Value: aws.String("e2e")},
			{Key: aws.String("suite"), Value: aws.String("rds")},
		},
	})
	require.NoError(t, err, "create-db-instance")
	require.NotNil(t, out.DBInstance)
	// Registered before the first wait, so an instance that never comes up is
	// still taken down rather than held against the next run's budget.
	t.Cleanup(func() { deleteInstance(t, f, id) })

	// The create returns before the engine exists, so the customer sees creating
	// and polls; the reconciler owns the flip to available.
	assert.Equal(t, harness.DBInstanceCreating, aws.StringValue(out.DBInstance.DBInstanceStatus))
	assert.Equal(t, id, aws.StringValue(out.DBInstance.DBInstanceIdentifier))

	var instance *rds.DBInstance
	t.Run("BecomesAvailable", func(t *testing.T) {
		instance = harness.WaitForDBInstanceAvailable(t, f.AWS, id)

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

	t.Run("EndpointResolves", func(t *testing.T) {
		requireAvailable(t, instance)
		host := aws.StringValue(instance.Endpoint.Address)

		// Without northstar the endpoint is the bare ENI IP, which is a valid
		// endpoint and needs no resolution.
		if ip := net.ParseIP(host); ip != nil {
			require.Empty(t, f.BaseDomain, "with northstar configured the endpoint must be a hostname, got %s", host)
			t.Logf("endpoint is the bare ENI IP %s (no base domain configured)", host)
			return
		}
		assert.Equal(t, fmt.Sprintf("%s.%s.%s.rds.%s", id, f.Account, f.Region, f.BaseDomain), host,
			"the endpoint name is account-qualified so identifiers collide across tenants without colliding in DNS")
		assertResolves(t, host)
	})

	t.Run("AcceptsAClientConnection", func(t *testing.T) {
		requireAvailable(t, instance)
		runSQLSmokeTest(t, instance)
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

func assertResolves(t *testing.T, host string) {
	t.Helper()
	// The record is published as soon as the ENI IP is known, well before the
	// instance is available, so a short envelope only covers zone propagation.
	harness.EventuallyErr(t, func() error {
		addrs, err := net.LookupHost(host)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", host, err)
		}
		if len(addrs) == 0 {
			return fmt.Errorf("lookup %s returned no addresses", host)
		}
		t.Logf("endpoint %s resolved to %v", host, addrs)
		return nil
	}, 90*time.Second, 3*time.Second)
}

// runSQLSmokeTest connects with the master credentials and writes a row, which
// is the only assertion that proves the engine actually bootstrapped rather than
// the agent merely reporting that it had. Shells to psql rather than linking a
// driver so the suite adds no production dependency; skips when psql is absent.
func runSQLSmokeTest(t *testing.T, instance *rds.DBInstance) {
	t.Helper()
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql not on PATH; skipping the client connection leg")
	}

	host := aws.StringValue(instance.Endpoint.Address)
	port := aws.Int64Value(instance.Endpoint.Port)
	table := "e2e_smoke"

	statements := strings.Join([]string{
		fmt.Sprintf("DROP TABLE IF EXISTS %s;", table),
		fmt.Sprintf("CREATE TABLE %s (id int primary key, note text);", table),
		fmt.Sprintf("INSERT INTO %s VALUES (1, 'hello from e2e');", table),
		fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", table),
	}, " ")

	// TLS is offered but not enforced (D14), so the smoke test does not pin
	// verify-full — the cert chain is the cert suite's concern.
	cmd := exec.Command(psql, //nolint:gosec // psql is LookPath-resolved, args test-controlled
		"--no-psqlrc", "--quiet", "--tuples-only", "--no-align",
		"--set", "ON_ERROR_STOP=1",
		"--host", host, "--port", fmt.Sprint(port),
		"--username", dbMasterUser, "--dbname", dbName,
		"--command", statements,
	)
	cmd.Env = append(cmd.Environ(), "PGPASSWORD="+dbMasterPassword, "PGCONNECT_TIMEOUT=30")

	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "psql against %s:%d: %s", host, port, output)
	assert.Contains(t, string(output), "hello from e2e", "the row written over the endpoint must read back")
}
