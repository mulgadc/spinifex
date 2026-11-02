//go:build e2e

package eks

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runNodegroupDelete proves DeleteNodegroup is asynchronous and leaves nothing
// behind that blocks the customer: the group answers DELETING first, then
// ResourceNotFound, after which the node role deletes with no hand step and an
// instance profile the customer made is untouched.
func runNodegroupDelete(t *testing.T, c *harness.AWSClient, fx *clusterFixture) {
	const trustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	roleName := fx.ClusterName + "-ngdel-node"
	roleOut, err := c.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
	})
	require.NoError(t, err, "create-node-role")
	roleDeleted := false
	t.Cleanup(func() {
		if !roleDeleted {
			_, _ = c.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)})
		}
	})

	fx.BystanderProfile = fx.ClusterName + "-bystander"
	_, err = c.IAM.CreateInstanceProfile(&iam.CreateInstanceProfileInput{InstanceProfileName: aws.String(fx.BystanderProfile)})
	require.NoError(t, err, "create-bystander-instance-profile")
	t.Cleanup(func() {
		_, _ = c.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(fx.BystanderProfile)})
	})

	const nodegroup = "ngdel-e2e-ng"
	harness.Phase(t, "Creating worker nodegroup %s", nodegroup)
	_, err = c.EKS.CreateNodegroup(&eks.CreateNodegroupInput{
		ClusterName:   aws.String(fx.ClusterName),
		NodegroupName: aws.String(nodegroup),
		Subnets:       aws.StringSlice([]string{fx.SubnetID}),
		NodeRole:      roleOut.Role.Arn,
		ScalingConfig: &eks.NodegroupScalingConfig{MinSize: aws.Int64(1), MaxSize: aws.Int64(1), DesiredSize: aws.Int64(1)},
	})
	require.NoError(t, err, "create-nodegroup")
	t.Cleanup(func() { harness.DeleteEKSNodegroupAndWait(t, c, fx.ClusterName, nodegroup) })
	describe := func() (*eks.Nodegroup, error) {
		out, derr := c.EKS.DescribeNodegroup(&eks.DescribeNodegroupInput{
			ClusterName: aws.String(fx.ClusterName), NodegroupName: aws.String(nodegroup),
		})
		if derr != nil {
			return nil, derr
		}
		return out.Nodegroup, nil
	}
	harness.EventuallyErr(t, func() error {
		ng, derr := describe()
		if derr != nil {
			return fmt.Errorf("describe-nodegroup: %w", derr)
		}
		if s := aws.StringValue(ng.Status); s != eks.NodegroupStatusActive {
			return fmt.Errorf("nodegroup status %q, want ACTIVE", s)
		}
		return nil
	}, 8*time.Minute, 10*time.Second)

	started := time.Now()
	out, err := c.EKS.DeleteNodegroup(&eks.DeleteNodegroupInput{
		ClusterName: aws.String(fx.ClusterName), NodegroupName: aws.String(nodegroup),
	})
	require.NoError(t, err, "delete-nodegroup")
	assert.Less(t, time.Since(started), 5*time.Second, "DeleteNodegroup must return without waiting for the workers")
	assert.Equal(t, eks.NodegroupStatusDeleting, aws.StringValue(out.Nodegroup.Status))
	ng, err := describe()
	require.NoError(t, err, "the group must still be described right after DeleteNodegroup returns")
	assert.Equal(t, eks.NodegroupStatusDeleting, aws.StringValue(ng.Status))

	harness.EventuallyErr(t, func() error {
		_, derr := describe()
		if derr == nil {
			return fmt.Errorf("nodegroup %s still exists", nodegroup)
		}
		if harness.ErrorCodeIs(derr, eks.ErrCodeResourceNotFoundException) {
			return nil
		}
		return fmt.Errorf("describe-nodegroup: %w", derr)
	}, 5*time.Minute, 3*time.Second)

	_, err = c.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	require.NoError(t, err, "delete-role must succeed once the nodegroup is gone, with no instance profile to detach by hand")
	roleDeleted = true
	_, err = c.IAM.GetInstanceProfile(&iam.GetInstanceProfileInput{InstanceProfileName: aws.String(fx.BystanderProfile)})
	require.NoError(t, err, "an instance profile the customer made must survive the nodegroup delete")
}
