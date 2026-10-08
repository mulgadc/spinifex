package subnetgroup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccount = "123456789012"
	testGroup   = "db-private"
)

type fakeSubnets struct{}

func (fakeSubnets) DescribeSubnets(_ context.Context, input *ec2.DescribeSubnetsInput, _ string) (*ec2.DescribeSubnetsOutput, error) {
	out := &ec2.DescribeSubnetsOutput{}
	for _, id := range aws.StringValueSlice(input.SubnetIds) {
		out.Subnets = append(out.Subnets, &ec2.Subnet{SubnetId: aws.String(id), VpcId: aws.String("vpc-a")})
	}
	return out, nil
}

type fakeDependants struct {
	users []string
	err   error
}

func (f fakeDependants) InstancesUsing(context.Context, string, string) ([]string, error) {
	return f.users, f.err
}

func newOwner(t *testing.T, subnets Subnets, dependants Dependants) *Owner {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	b := kvstore.NewBucket(js, kvstore.Config{Name: "rds-account-" + testAccount, History: 1})
	return New(subnets, dependants, func(context.Context, string) (*kvstore.Bucket, error) { return b, nil })
}

func TestKey_SitsUnderPrefix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "db-subnet-groups/prod-db-subnets", Key("prod-db-subnets"))
	assert.True(t, strings.HasPrefix(Key("x"), Prefix()))
}

// The in-use guard fails closed: when the instance side cannot answer, the
// group stays rather than being deleted on an unknown answer.
func TestDelete_KeepsTheGroupWhenTheInUseCheckFails(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeSubnets{}, fakeDependants{err: errors.New("instance scan failed")})
	_, err := owner.Create(t.Context(), testAccount, Spec{Name: testGroup, Description: "d", SubnetIDs: []string{"subnet-a"}})
	require.NoError(t, err)

	require.ErrorContains(t, owner.Delete(t.Context(), testAccount, testGroup), "instance scan failed")
	_, err = owner.Get(t.Context(), testAccount, testGroup)
	require.NoError(t, err)
}

func TestDelete_RefusesWhileDependantsNameTheGroup(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeSubnets{}, fakeDependants{users: []string{"orders-db", "users-db"}})
	_, err := owner.Create(t.Context(), testAccount, Spec{Name: testGroup, Description: "d", SubnetIDs: []string{"subnet-a"}})
	require.NoError(t, err)

	err = owner.Delete(t.Context(), testAccount, testGroup)
	assert.Equal(t, awserrors.ErrorDBSubnetGroupInvalidState, awserrors.ValidErrorCodeFromError(err))
	assert.ErrorContains(t, err, "orders-db, users-db")
}

// A node without RDS networking cannot validate members, so it stores nothing.
func TestCreate_RefusesWithoutSubnetResolution(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, nil, fakeDependants{})
	_, err := owner.Create(t.Context(), testAccount, Spec{Name: testGroup, Description: "d", SubnetIDs: []string{"subnet-a"}})
	assert.Equal(t, awserrors.ErrorServerInternal, awserrors.ValidErrorCodeFromError(err))

	_, err = owner.Get(t.Context(), testAccount, testGroup)
	assert.Equal(t, awserrors.ErrorDBSubnetGroupNotFound, awserrors.ValidErrorCodeFromError(err))
}

// Placement reports member subnets in request order; choosing among them is the instance's job.
func TestPlacement_ReportsTheVPCAndMembers(t *testing.T) {
	t.Parallel()
	owner := newOwner(t, fakeSubnets{}, fakeDependants{})
	_, err := owner.Create(t.Context(), testAccount, Spec{Name: testGroup, Description: "d", SubnetIDs: []string{"subnet-z", "subnet-a"}})
	require.NoError(t, err)

	vpcID, ids, err := owner.Placement(t.Context(), testAccount, testGroup)
	require.NoError(t, err)
	assert.Equal(t, "vpc-a", vpcID)
	assert.Equal(t, []string{"subnet-z", "subnet-a"}, ids)
}
