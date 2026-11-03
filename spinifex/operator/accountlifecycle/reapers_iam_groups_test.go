package accountlifecycle

//test:in-package — drives the unexported identity reapers against the real IAM
// service, so the DeleteConflict refusals are the real ones and not a fake's.

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const groupTestAccount = "000000000042"

const groupTestPolicyDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:ListAllMyBuckets","Resource":"*"}]}`

func newRealIAM(t *testing.T) *iamdomain.IAMServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)
	return svc
}

// drainIdentity runs the identity-stage reapers in registration order for a
// few passes, as a stage does, and returns what each reaper still lists and
// the delete errors of the last pass.
func drainIdentity(t *testing.T, svc iamdomain.IAMService) (remaining []Resource, lastErrs []error) {
	t.Helper()
	for range 3 {
		lastErrs = nil
		for _, reaper := range IAMReapers(svc) {
			if reaper.Stage() != StageIdentity {
				continue
			}
			found, err := reaper.List(testCtx(t), groupTestAccount)
			require.NoError(t, err)
			for _, resource := range found {
				if err := reaper.Delete(testCtx(t), groupTestAccount, resource, false); err != nil {
					lastErrs = append(lastErrs, err)
				}
			}
		}
	}
	for _, reaper := range IAMReapers(svc) {
		if reaper.Stage() != StageIdentity {
			continue
		}
		found, err := reaper.List(testCtx(t), groupTestAccount)
		require.NoError(t, err)
		remaining = append(remaining, found...)
	}
	return remaining, lastErrs
}

func seedGroupAccount(t *testing.T, svc *iamdomain.IAMServiceImpl) string {
	t.Helper()
	_, err := svc.CreateUser(groupTestAccount, &iam.CreateUserInput{UserName: aws.String("u")})
	require.NoError(t, err)
	_, err = svc.CreateGroup(groupTestAccount, &iam.CreateGroupInput{GroupName: aws.String("g")})
	require.NoError(t, err)
	_, err = svc.AddUserToGroup(groupTestAccount, &iam.AddUserToGroupInput{UserName: aws.String("u"), GroupName: aws.String("g")})
	require.NoError(t, err)
	policy, err := svc.CreatePolicy(groupTestAccount, &iam.CreatePolicyInput{
		PolicyName: aws.String("p"), PolicyDocument: aws.String(groupTestPolicyDoc),
	})
	require.NoError(t, err)
	_, err = svc.AttachGroupPolicy(groupTestAccount, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("g"), PolicyArn: policy.Policy.Arn,
	})
	require.NoError(t, err)
	_, err = svc.PutGroupPolicy(groupTestAccount, &iam.PutGroupPolicyInput{
		GroupName: aws.String("g"), PolicyName: aws.String("inline"), PolicyDocument: aws.String(groupTestPolicyDoc),
	})
	require.NoError(t, err)
	return *policy.Policy.Arn
}

// A user in a group, a group carrying an attached and an inline policy, and the
// customer policy attached to it all drain; none of them may strand the account.
func TestIdentityStageDrainsGroupsMembersAndGroupPolicies(t *testing.T) {
	svc := newRealIAM(t)
	seedGroupAccount(t, svc)

	remaining, lastErrs := drainIdentity(t, svc)

	assert.Empty(t, remaining)
	assert.Empty(t, lastErrs)
}
