//go:build e2e

package rds

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthorization drives the four layers that stand between a caller and a DB
// instance, each with the narrowest principal that can prove it: an action-level
// grant, the internal-action principal gate, a resource-scoped grant, and tenant
// isolation.
//
// It needs its own principals because the suite's own credentials are an
// account administrator, against which every one of these checks passes.
func TestAuthorization(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	// The instance under test, plus the second tenant's short-lived twin.
	reserveDBVMs(t, dbClass, dbClass)

	suffix := time.Now().Unix()
	id := fmt.Sprintf("%s-authz-%d", dbInstancePfx, suffix)

	harness.Phase(t, "Creating DB instance %q for the authorization matrix", id)
	createDBInstance(t, f, id)
	instance := waitForAvailable(t, f, id)
	arn := aws.StringValue(instance.DBInstanceArn)
	require.NotEmpty(t, arn, "an available instance must publish its ARN")

	// The grant a read-only dashboard or a monitoring integration would carry.
	t.Run("ADescribeGrantReadsButCannotCreate", func(t *testing.T) {
		describer := scopedPrincipal(t, f, "rds-e2e-describer", policyStatement{
			Action: []string{"rds:Describe*"}, Resource: []string{"*"},
		})

		listed, err := describer.RDS.DescribeDBInstances(&rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(id),
		})
		require.NoError(t, err, "rds:Describe* must allow describe-db-instances")
		require.Len(t, listed.DBInstances, 1)

		expectCreateRefused(t, f, describer, "AccessDenied",
			validCreateInput(fmt.Sprintf("%s-denied-%d", dbInstancePfx, suffix)))
	})

	// The internal actions are refused by principal class, before any policy is
	// read: GetDBBootstrapConfig hands back the master password (D8), so rds:* —
	// a legitimate administrator grant — must not reach it. Asserted with a
	// principal that holds rds:*, or the denial would only prove a missing grant.
	t.Run("RDSStarCannotReachAnInternalAction", func(t *testing.T) {
		operator := scopedPrincipal(t, f, "rds-e2e-operator", policyStatement{
			Action: []string{"rds:*"}, Resource: []string{"*"},
		})

		_, err := operator.RDS.DescribeDBInstances(&rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(id),
		})
		require.NoError(t, err, "rds:* must allow a customer action")

		status, body, code := harness.PostRDSAction(t, f.Env, operator, "GetDBBootstrapConfig", map[string]string{
			"DBInstanceIdentifier": id,
		})
		assert.Equal(t, 403, status, "body: %s", body)
		assert.Equal(t, "AccessDenied", code, "body: %s", body)
	})

	// A tenant sees nothing of another tenant's instances, and the namespace is
	// per-account rather than global: two tenants may hold the same identifier.
	t.Run("AnotherTenantSeesNothing", func(t *testing.T) {
		carousel := harness.NewAccountCarousel()
		tenant := carousel.Add(t, f.Env, "rds-e2e-tenant",
			harness.SpxAdminAccountCreate(t, fmt.Sprintf("RDS E2E Tenant %d", suffix), ""))
		other := tenant.Client

		// Not AccessDenied: the instance is not in this tenant's namespace at all,
		// and an administrator of their own account is not being refused a grant.
		for _, tc := range []struct {
			name string
			call func() error
		}{
			{"Describe", func() error {
				_, err := harness.DescribeDBInstance(other, id)
				return err
			}},
			{"Stop", func() error {
				_, err := other.RDS.StopDBInstance(&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
				return err
			}},
			{"Snapshot", func() error {
				_, err := other.RDS.CreateDBSnapshot(&rds.CreateDBSnapshotInput{
					DBInstanceIdentifier: aws.String(id),
					DBSnapshotIdentifier: aws.String(id + "-cross-account"),
				})
				return err
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				harness.ExpectError(t, "DBInstanceNotFound", tc.call)
			})
		}

		// The same identifier in both accounts. Created and deleted without ever
		// being waited for: what is under test is the record's namespace, not the
		// engine behind it.
		t.Run("TheSameIdentifierIsADifferentInstance", func(t *testing.T) {
			twin, err := other.RDS.CreateDBInstance(validCreateInput(id)) //nolint:staticcheck // e2e:allow-create — the second tenant's own instance
			require.NoError(t, err, "an identifier held by another tenant must still be available here")
			t.Cleanup(func() { deleteInstanceAs(t, other, id) })

			assert.Equal(t, tenant.AccountID, arnAccount(aws.StringValue(twin.DBInstance.DBInstanceArn)),
				"the twin's ARN must name its own account")
			assert.NotEqual(t, arn, aws.StringValue(twin.DBInstance.DBInstanceArn))

			// The original is untouched by the twin's create, and still reports the
			// state its own tenant left it in.
			mine, err := harness.DescribeDBInstance(f.AWS, id)
			require.NoError(t, err)
			assert.Equal(t, arn, aws.StringValue(mine.DBInstanceArn))
			assert.Equal(t, harness.DBInstanceAvailable, aws.StringValue(mine.DBInstanceStatus))
		})
	})

	// Last, because the grant it proves is exercised by actually stopping the
	// instance: a stop that returns success and does nothing is the failure this
	// subtest is here to catch.
	t.Run("AResourceScopedGrantAppliesToOneInstanceOnly", func(t *testing.T) {
		stopper := scopedPrincipal(t, f, "rds-e2e-stopper", policyStatement{
			Action: []string{"rds:StopDBInstance"}, Resource: []string{arn},
		})

		// AccessDenied rather than DBInstanceNotFound: the resource check has to run
		// ahead of existence, or a denial would tell a caller which identifiers are
		// taken.
		harness.ExpectError(t, "AccessDenied", func() error {
			_, err := stopper.RDS.StopDBInstance(&rds.StopDBInstanceInput{
				DBInstanceIdentifier: aws.String(id + "-elsewhere"),
			})
			return err
		})

		harness.Phase(t, "Stopping %q as the resource-scoped principal", id)
		_, err := stopper.RDS.StopDBInstance(&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
		require.NoError(t, err, "the grant names this instance's ARN, so the stop must be allowed")
		harness.WaitForDBInstanceStatus(t, f.AWS, id, harness.DBInstanceStopped)
	})
}

// One Allow statement of an inline policy.
type policyStatement struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// scopedPrincipal returns a client for a fresh user in the suite's own account
// carrying exactly the statements given and nothing else, which is the only way
// to assert a grant: the suite's own principal is an administrator, so every
// denial has to be proven against a narrower one.
func scopedPrincipal(t *testing.T, f *Fixture, name string, statements ...policyStatement) *harness.AWSClient {
	t.Helper()
	// Suffixed so a failed run's leftovers cannot be mistaken for this run's
	// principal, which would silently assert against the wrong grant.
	userName := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())

	_, err := f.AWS.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-user %s", userName)
	t.Cleanup(func() {
		if _, err := f.AWS.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(userName)}); err != nil {
			t.Logf("cleanup: delete-user %s: %v", userName, err)
		}
	})

	for i := range statements {
		statements[i].Effect = "Allow"
	}
	document, err := json.Marshal(map[string]any{
		"Version":   "2012-10-17",
		"Statement": statements,
	})
	require.NoError(t, err, "marshal policy document")

	const policyName = "rds-e2e-grant"
	_, err = f.AWS.IAM.PutUserPolicy(&iam.PutUserPolicyInput{
		UserName:       aws.String(userName),
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(string(document)),
	})
	require.NoError(t, err, "put-user-policy %s", userName)
	t.Cleanup(func() {
		if _, err := f.AWS.IAM.DeleteUserPolicy(&iam.DeleteUserPolicyInput{
			UserName:   aws.String(userName),
			PolicyName: aws.String(policyName),
		}); err != nil {
			t.Logf("cleanup: delete-user-policy %s: %v", userName, err)
		}
	})

	key, err := f.AWS.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-access-key %s", userName)
	t.Cleanup(func() {
		if _, err := f.AWS.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{
			UserName:    aws.String(userName),
			AccessKeyId: key.AccessKey.AccessKeyId,
		}); err != nil {
			t.Logf("cleanup: delete-access-key %s: %v", userName, err)
		}
	})

	client := harness.NewAWSClientWithCreds(t, f.Env,
		aws.StringValue(key.AccessKey.AccessKeyId), aws.StringValue(key.AccessKey.SecretAccessKey))
	// A key is not necessarily live the instant CreateAccessKey returns, and a
	// failure to authenticate is a different answer from a denial. Waiting for the
	// key to authenticate keeps a propagation delay from reading as an
	// authorization result.
	harness.EventuallyErr(t, func() error {
		_, err := client.RDS.DescribeDBInstances(&rds.DescribeDBInstancesInput{})
		if err == nil || harness.ErrorCodeIs(err, "AccessDenied") {
			return nil
		}
		return fmt.Errorf("credentials for %s are not live yet: %w", userName, err)
	}, 60*time.Second, 2*time.Second)
	return client
}

// The account segment of an ARN, which is what makes two identical identifiers
// in two accounts distinguishable.
func arnAccount(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 5 {
		return ""
	}
	return parts[4]
}
