package gateway

import (
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/stretchr/testify/require"
)

func TestAWSOperationInventoryClassifiesDispatchers(t *testing.T) {
	inventory := AWSOperationInventory(ecrRegistrationInventory())

	require.Contains(t, inventory["ec2"].Registered, "DescribeInstances")
	require.NotContains(t, inventory["ec2"].Stubbed, "DescribeInstances")

	require.Contains(t, inventory["ecs"].Registered, "DescribeClusters")
	require.NotContains(t, inventory["ecs"].Stubbed, "DescribeClusters")
	require.Contains(t, inventory["ecs"].Stubbed, "UpdateCluster")

	// These operations are NotImplemented in the relay table but intercepted
	// by a composed capability the ECR registration declares.
	require.Contains(t, inventory["ecr"].Registered, "GetAuthorizationToken")
	require.NotContains(t, inventory["ecr"].Stubbed, "GetAuthorizationToken")
	require.NotContains(t, inventory["ecr"].Stubbed, "DescribeRepositories")
	require.Contains(t, inventory["ecr"].Unsupported, "StartImageScan")

	require.Contains(t, inventory["eks"].Registered, "DescribeCluster")
	require.NotContains(t, inventory["eks"].Stubbed, "DescribeCluster")

	require.Contains(t, inventory["rds"].Registered, "CreateDBInstance")
	require.NotContains(t, inventory["rds"].Unsupported, "CreateDBInstance")
	require.Contains(t, inventory["rds"].Unsupported, "CreateDBCluster")

	_, hasS3 := inventory["s3"]
	require.False(t, hasS3, "S3 is delegated to Predastore, not dispatched here")
}

func TestAWSOperationInventoryMeasuresEKSAgainstTheModel(t *testing.T) {
	inventory := AWSOperationInventory(ecrRegistrationInventory())

	eks, err := awsmodel.CompareOperations(awsmodel.EKS, awsmodel.DispatchInventory{
		Registered:  inventory["eks"].Registered,
		Stubbed:     inventory["eks"].Stubbed,
		Unsupported: inventory["eks"].Unsupported,
	})
	require.NoError(t, err)
	require.Contains(t, eks.Implemented, "DescribeCluster")
	// Spinifex-internal control-plane routes have no AWS model entry, so they
	// must be reported as registered outside the pinned model.
	require.ElementsMatch(t, []string{
		"GetRecoveryDirective",
		"ListInternalAddons",
		"PublishInternal",
		"WebhookTokenReview",
	}, eks.Extra)
	for _, action := range eks.Extra {
		require.NotContains(t, eks.Implemented, action)
	}
}

func TestEKSActionNamesAreSortedAndDistinct(t *testing.T) {
	names := eksActionNames()

	require.True(t, slices.IsSorted(names), "actions are not sorted: %v", names)
	require.Len(t, slices.Compact(slices.Clone(names)), len(names), "actions contain duplicates: %v", names)
}

func TestAWSOperationInventoryStatusesAreDisjointSubsets(t *testing.T) {
	for service, inventory := range AWSOperationInventory(ecrRegistrationInventory()) {
		t.Run(service, func(t *testing.T) {
			registered := stringSet(inventory.Registered)
			for _, action := range inventory.Stubbed {
				require.True(t, registered[action], "stubbed action %q is not registered", action)
				require.NotContains(t, inventory.Unsupported, action, "action cannot be stubbed and unsupported")
			}
			for _, action := range inventory.Unsupported {
				require.True(t, registered[action], "unsupported action %q is not registered", action)
			}
		})
	}
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}
