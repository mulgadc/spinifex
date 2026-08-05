package gateway

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAWSOperationInventoryClassifiesDispatchers(t *testing.T) {
	inventory := AWSOperationInventory()

	require.Contains(t, inventory["ec2"].Registered, "DescribeInstances")
	require.NotContains(t, inventory["ec2"].Stubbed, "DescribeInstances")

	require.Contains(t, inventory["ecs"].Registered, "DescribeClusters")
	require.NotContains(t, inventory["ecs"].Stubbed, "DescribeClusters")
	require.Contains(t, inventory["ecs"].Stubbed, "UpdateCluster")

	// These operations are NotImplemented in the relay table but intercepted
	// by ECR_Request's authoritative inline dispatch map.
	require.Contains(t, inventory["ecr"].Registered, "GetAuthorizationToken")
	require.NotContains(t, inventory["ecr"].Stubbed, "GetAuthorizationToken")
	require.NotContains(t, inventory["ecr"].Stubbed, "DescribeRepositories")
	require.Contains(t, inventory["ecr"].Unsupported, "StartImageScan")

	_, hasS3 := inventory["s3"]
	require.False(t, hasS3, "S3 is delegated to Predastore, not dispatched here")
}

func TestAWSOperationInventoryStatusesAreDisjointSubsets(t *testing.T) {
	for service, inventory := range AWSOperationInventory() {
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
