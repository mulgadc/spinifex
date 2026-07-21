//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/stretchr/testify/require"
)

// Identifiers for the IAM User/AccessKey/Policy suite. Matching the E2E
// source's names keeps a future side-by-side diff greppable.
const (
	iamUserAlice   = "alice"
	iamUserBob     = "bob"
	iamUserCharlie = "charlie"
	iamUserBobPath = "/engineering/"

	iamPolicyEC2ReadOnly    = "EC2ReadOnly"
	iamPolicyFullAdmin      = "FullAdmin"
	iamPolicyFullAdminPath  = "/admin/"
	iamPolicyDenyTerminate  = "DenyTerminate"
	iamPolicyIAMReadOnly    = "IAMReadOnly"
	iamPolicyEC2DescribeAll = "EC2DescribeAll"
)

// Inline policy documents mirroring the E2E source.
const (
	iamDocEC2ReadOnly = `{
        "Version": "2012-10-17",
        "Statement": [{
            "Effect": "Allow",
            "Action": ["ec2:DescribeInstances", "ec2:DescribeVolumes", "ec2:DescribeVpcs"],
            "Resource": "*"
        }]
    }`

	iamDocFullAdmin = `{
        "Version": "2012-10-17",
        "Statement": [{
            "Effect": "Allow",
            "Action": "*",
            "Resource": "*"
        }]
    }`

	iamDocDenyTerminate = `{
        "Version": "2012-10-17",
        "Statement": [
            {"Effect": "Allow", "Action": "ec2:*", "Resource": "*"},
            {"Effect": "Deny", "Action": "ec2:TerminateInstances", "Resource": "*"}
        ]
    }`

	iamDocIAMReadOnly = `{
        "Version": "2012-10-17",
        "Statement": [{
            "Effect": "Allow",
            "Action": ["iam:GetUser", "iam:ListUsers", "iam:ListPolicies", "iam:GetPolicy"],
            "Resource": "*"
        }]
    }`

	iamDocEC2DescribeAll = `{
        "Version": "2012-10-17",
        "Statement": [{
            "Effect": "Allow",
            "Action": "ec2:Describe*",
            "Resource": "*"
        }]
    }`
)

// iamPolicyARN builds arn:aws:iam::<account>:policy/<key> for constructing a
// deliberately-nonexistent policy ARN in negative-path assertions. Shared by
// every file in this package that needs to address a policy that must not
// resolve.
func iamPolicyARN(account, key string) string {
	return "arn:aws:iam::" + account + ":policy/" + key
}

// iamRoleARN builds arn:aws:iam::<account>:role/<name>.
func iamRoleARN(account, name string) string {
	return "arn:aws:iam::" + account + ":role/" + name
}

// iamGroupARN builds arn:aws:iam::<account>:group/<name> (default path /).
func iamGroupARN(account, name string) string {
	return "arn:aws:iam::" + account + ":group/" + name
}

// iamFindKeyStatus returns the Status of a specific access key for a user.
// Fails the test if the key isn't found — the caller has already asserted
// the key exists.
func iamFindKeyStatus(t *testing.T, iamCli *iam.IAM, user, keyID string) string {
	t.Helper()
	out, err := iamCli.ListAccessKeys(&iam.ListAccessKeysInput{UserName: aws.String(user)})
	require.NoError(t, err, "list-access-keys %s", user)
	for _, k := range out.AccessKeyMetadata {
		if aws.StringValue(k.AccessKeyId) == keyID {
			return aws.StringValue(k.Status)
		}
	}
	t.Fatalf("access key %s not found for user %s", keyID, user)
	return ""
}

// TestIAMUserCRUD proves CreateUser/GetUser/ListUsers/DeleteUser plus the
// EntityAlreadyExists / NoSuchEntity negative paths, and DeleteUser
// idempotency (a second delete surfaces NoSuchEntity rather than
// silently succeeding). All IAM actions dispatch straight to gw.IAMService
// with no NATS hop, so this needs no stub.
func TestIAMUserCRUD(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)

	// SeedBootstrap always seeds a root user, so a fresh gateway's account is
	// never truly empty.
	rootUsers, err := iamCli.ListUsers(&iam.ListUsersInput{})
	require.NoError(t, err, "list-users (root)")
	require.NotEmpty(t, rootUsers.Users, "root list-users returned 0 — root account missing?")

	aliceOut, err := iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-user %s", iamUserAlice)
	require.Equal(t, iamUserAlice, aws.StringValue(aliceOut.User.UserName))

	bobOut, err := iamCli.CreateUser(&iam.CreateUserInput{
		UserName: aws.String(iamUserBob),
		Path:     aws.String(iamUserBobPath),
	})
	require.NoError(t, err, "create-user %s", iamUserBob)
	require.Equal(t, iamUserBobPath, aws.StringValue(bobOut.User.Path), "bob created without expected path")

	_, err = iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserAlice)})
	requireAWSErrorCode(t, err, "EntityAlreadyExists")

	got, err := iamCli.GetUser(&iam.GetUserInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "get-user %s", iamUserAlice)
	require.Equal(t, iamUserAlice, aws.StringValue(got.User.UserName))

	_, err = iamCli.GetUser(&iam.GetUserInput{UserName: aws.String("nonexistent")})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	listed, err := iamCli.ListUsers(&iam.ListUsersInput{})
	require.NoError(t, err, "list-users")
	require.GreaterOrEqual(t, len(listed.Users), 3, "expected >=3 users (root+alice+bob), got %d", len(listed.Users))

	eng, err := iamCli.ListUsers(&iam.ListUsersInput{PathPrefix: aws.String(iamUserBobPath)})
	require.NoError(t, err, "list-users --path-prefix")
	require.Len(t, eng.Users, 1, "expected 1 user under %s, got %d", iamUserBobPath, len(eng.Users))

	// DeleteUser idempotency: create a throwaway user, delete it, then delete
	// it again and assert the second delete surfaces NoSuchEntity rather than
	// silently succeeding (matches AWS semantics).
	const ephemeral = "iam-phase1-ephemeral"
	_, err = iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(ephemeral)})
	require.NoError(t, err, "create-user %s", ephemeral)

	_, err = iamCli.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(ephemeral)})
	require.NoError(t, err, "delete-user %s", ephemeral)

	_, err = iamCli.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(ephemeral)})
	requireAWSErrorCode(t, err, "NoSuchEntity")
}

// TestIAMAccessKeyLifecycle proves CreateAccessKey, ListAccessKeys,
// UpdateAccessKey (deactivate/activate), DeleteAccessKey plus the
// LimitExceeded (2-key cap) / NoSuchEntity negative paths. No NATS stub
// needed — access keys are pure IAM state.
func TestIAMAccessKeyLifecycle(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)

	_, err := iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-user alice")
	_, err = iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserBob)})
	require.NoError(t, err, "create-user bob")

	k1, err := iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-access-key 1")
	aliceKeyID := aws.StringValue(k1.AccessKey.AccessKeyId)
	require.NotEmpty(t, aliceKeyID, "empty AccessKeyId")
	require.NotEmpty(t, aws.StringValue(k1.AccessKey.SecretAccessKey), "empty SecretAccessKey")

	k2, err := iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-access-key 2")
	aliceKey2 := aws.StringValue(k2.AccessKey.AccessKeyId)
	require.NotEmpty(t, aliceKey2)

	_, err = iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(iamUserAlice)})
	requireAWSErrorCode(t, err, "LimitExceeded")

	_, err = iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String("ghost")})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	keys, err := iamCli.ListAccessKeys(&iam.ListAccessKeysInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "list-access-keys alice")
	require.Len(t, keys.AccessKeyMetadata, 2, "alice key count")

	bobKeys, err := iamCli.ListAccessKeys(&iam.ListAccessKeysInput{UserName: aws.String(iamUserBob)})
	require.NoError(t, err, "list-access-keys bob")
	require.Empty(t, bobKeys.AccessKeyMetadata, "bob should have 0 keys")

	_, err = iamCli.UpdateAccessKey(&iam.UpdateAccessKeyInput{
		UserName:    aws.String(iamUserAlice),
		AccessKeyId: aws.String(aliceKeyID),
		Status:      aws.String(iam.StatusTypeInactive),
	})
	require.NoError(t, err, "update-access-key deactivate")
	require.Equal(t, iam.StatusTypeInactive, iamFindKeyStatus(t, iamCli, iamUserAlice, aliceKeyID),
		"key not Inactive after update")

	_, err = iamCli.UpdateAccessKey(&iam.UpdateAccessKeyInput{
		UserName:    aws.String(iamUserAlice),
		AccessKeyId: aws.String(aliceKeyID),
		Status:      aws.String(iam.StatusTypeActive),
	})
	require.NoError(t, err, "update-access-key reactivate")

	_, err = iamCli.DeleteAccessKey(&iam.DeleteAccessKeyInput{
		UserName:    aws.String(iamUserAlice),
		AccessKeyId: aws.String(aliceKey2),
	})
	require.NoError(t, err, "delete-access-key key2")

	after, err := iamCli.ListAccessKeys(&iam.ListAccessKeysInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "list-access-keys after delete")
	require.Len(t, after.AccessKeyMetadata, 1, "alice should have 1 key after delete")
}

// TestIAMUserAuthentication builds a scoped AWS client with alice's key and
// confirms signing is honoured (active key) / rejected (deactivated key, bad
// secret, bogus ID). Alice has no policy, so the authenticated call is
// authz-denied — AccessDenied (not an authn error) proves the active key
// signed and was accepted. The final root DescribeInstances call needs the
// EC2 daemon subjects stubbed since it actually dispatches over NATS; every
// other assertion here is rejected before dispatch.
func TestIAMUserAuthentication(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)
	stubEmptyInstanceBuckets(t, gw)
	gw.StubSubject(t, "ec2.DescribeInstances", mustMarshal(t, &ec2.DescribeInstancesOutput{}))

	_, err := iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-user alice")
	k, err := iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-access-key alice")
	aliceKeyID := aws.StringValue(k.AccessKey.AccessKeyId)
	aliceSecret := aws.StringValue(k.AccessKey.SecretAccessKey)

	// Active, correctly-signed key with no policy: the request authenticates
	// then default-denies. AccessDenied (vs the InvalidClientTokenId /
	// SignatureDoesNotMatch below) is the signal that authn succeeded.
	aliceCli := gw.ClientsWithCreds(t, aliceKeyID, aliceSecret)
	_, err = aliceCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	_, err = iamCli.UpdateAccessKey(&iam.UpdateAccessKeyInput{
		UserName:    aws.String(iamUserAlice),
		AccessKeyId: aws.String(aliceKeyID),
		Status:      aws.String(iam.StatusTypeInactive),
	})
	require.NoError(t, err, "deactivate alice key")
	_, err = aliceCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	requireAWSErrorCode(t, err, "InvalidClientTokenId")

	_, err = iamCli.UpdateAccessKey(&iam.UpdateAccessKeyInput{
		UserName:    aws.String(iamUserAlice),
		AccessKeyId: aws.String(aliceKeyID),
		Status:      aws.String(iam.StatusTypeActive),
	})
	require.NoError(t, err, "reactivate alice key")

	badSecretCli := gw.ClientsWithCreds(t, aliceKeyID, "WRONG_SECRET_KEY_HERE_12345678901")
	_, err = badSecretCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	requireAWSErrorCode(t, err, "SignatureDoesNotMatch")

	fakeCli := gw.ClientsWithCreds(t, "AKIAXXXXXXXXXXXXXXXX", "doesntmatter")
	_, err = fakeCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	requireAWSErrorCode(t, err, "InvalidClientTokenId")

	_, err = gw.EC2Client(t).DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "root describe-instances")
}

// TestIAMPolicyCRUD proves CreatePolicy (5 variants), GetPolicy,
// GetPolicyVersion, ListPolicies plus the EntityAlreadyExists /
// MalformedPolicyDocument / NoSuchEntity negative paths. No NATS stub
// needed — policies are pure IAM state.
func TestIAMPolicyCRUD(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)

	pol, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(iamPolicyEC2ReadOnly),
		PolicyDocument: aws.String(iamDocEC2ReadOnly),
	})
	require.NoError(t, err, "create-policy %s", iamPolicyEC2ReadOnly)
	ec2roArn := aws.StringValue(pol.Policy.Arn)
	require.NotEmpty(t, ec2roArn, "empty policy ARN")

	// The remaining four policies have no cross-dependency, so fan them out
	// in parallel under a wrapping t.Run that blocks until all four complete.
	t.Run("create_policies_parallel", func(t *testing.T) {
		t.Run(iamPolicyFullAdmin, func(t *testing.T) {
			t.Parallel()
			_, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
				PolicyName:     aws.String(iamPolicyFullAdmin),
				Path:           aws.String(iamPolicyFullAdminPath),
				Description:    aws.String("Full access to all services"),
				PolicyDocument: aws.String(iamDocFullAdmin),
			})
			require.NoError(t, err, "create-policy %s", iamPolicyFullAdmin)
		})
		t.Run(iamPolicyDenyTerminate, func(t *testing.T) {
			t.Parallel()
			_, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
				PolicyName:     aws.String(iamPolicyDenyTerminate),
				PolicyDocument: aws.String(iamDocDenyTerminate),
			})
			require.NoError(t, err, "create-policy %s", iamPolicyDenyTerminate)
		})
		t.Run(iamPolicyIAMReadOnly, func(t *testing.T) {
			t.Parallel()
			_, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
				PolicyName:     aws.String(iamPolicyIAMReadOnly),
				PolicyDocument: aws.String(iamDocIAMReadOnly),
			})
			require.NoError(t, err, "create-policy %s", iamPolicyIAMReadOnly)
		})
		t.Run(iamPolicyEC2DescribeAll, func(t *testing.T) {
			t.Parallel()
			_, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
				PolicyName:     aws.String(iamPolicyEC2DescribeAll),
				PolicyDocument: aws.String(iamDocEC2DescribeAll),
			})
			require.NoError(t, err, "create-policy %s", iamPolicyEC2DescribeAll)
		})
	})

	_, err = iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(iamPolicyEC2ReadOnly),
		PolicyDocument: aws.String(iamDocFullAdmin),
	})
	requireAWSErrorCode(t, err, "EntityAlreadyExists")

	_, err = iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String("BadPolicy"),
		PolicyDocument: aws.String(`{"not valid"}`),
	})
	requireAWSErrorCode(t, err, "MalformedPolicyDocument")

	got, err := iamCli.GetPolicy(&iam.GetPolicyInput{PolicyArn: aws.String(ec2roArn)})
	require.NoError(t, err, "get-policy %s", iamPolicyEC2ReadOnly)
	require.Equal(t, iamPolicyEC2ReadOnly, aws.StringValue(got.Policy.PolicyName))

	_, err = iamCli.GetPolicy(&iam.GetPolicyInput{PolicyArn: aws.String(iamPolicyARN(gw.AccountID, "Ghost"))})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	pv, err := iamCli.GetPolicyVersion(&iam.GetPolicyVersionInput{
		PolicyArn: aws.String(ec2roArn),
		VersionId: aws.String("v1"),
	})
	require.NoError(t, err, "get-policy-version v1")
	require.Equal(t, "v1", aws.StringValue(pv.PolicyVersion.VersionId))

	all, err := iamCli.ListPolicies(&iam.ListPoliciesInput{})
	require.NoError(t, err, "list-policies")
	require.GreaterOrEqual(t, len(all.Policies), 5, "expected >=5 policies, got %d", len(all.Policies))
}

// TestIAMPolicyAttachmentEnforcement proves AttachUserPolicy idempotency,
// ListAttachedUserPolicies, and enforcement across default-deny, explicit
// allow, wildcard-allow + explicit-deny, root bypass, prefix-wildcard and
// FullAdmin, then DetachUserPolicy. DescribeRegions isn't used here — the
// canary actions are DescribeInstances/DescribeVpcs/DescribeKeyPairs, all of
// which dispatch over NATS, so the allow-path calls need their daemon
// subjects stubbed (deny-path calls never reach NATS).
func TestIAMPolicyAttachmentEnforcement(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)
	adminAccount := gw.AccountID

	stubEmptyInstanceBuckets(t, gw)
	gw.StubSubject(t, "ec2.DescribeInstances", mustMarshal(t, &ec2.DescribeInstancesOutput{}))
	gw.StubSubject(t, "ec2.DescribeVpcs", mustMarshal(t, &ec2.DescribeVpcsOutput{}))
	gw.StubSubject(t, "ec2.DescribeKeyPairs", mustMarshal(t, &ec2.DescribeKeyPairsOutput{}))

	_, err := iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-user alice")
	_, err = iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserBob)})
	require.NoError(t, err, "create-user bob")
	_, err = iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserCharlie)})
	require.NoError(t, err, "create-user charlie")

	aliceKey, err := iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-access-key alice")
	bobKey, err := iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(iamUserBob)})
	require.NoError(t, err, "create-access-key bob")
	charlieKey, err := iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(iamUserCharlie)})
	require.NoError(t, err, "create-access-key charlie")

	aliceCli := gw.ClientsWithCreds(t, aws.StringValue(aliceKey.AccessKey.AccessKeyId), aws.StringValue(aliceKey.AccessKey.SecretAccessKey))
	bobCli := gw.ClientsWithCreds(t, aws.StringValue(bobKey.AccessKey.AccessKeyId), aws.StringValue(bobKey.AccessKey.SecretAccessKey))
	charlieCli := gw.ClientsWithCreds(t, aws.StringValue(charlieKey.AccessKey.AccessKeyId), aws.StringValue(charlieKey.AccessKey.SecretAccessKey))

	ec2roPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName: aws.String(iamPolicyEC2ReadOnly), PolicyDocument: aws.String(iamDocEC2ReadOnly),
	})
	require.NoError(t, err, "create-policy EC2ReadOnly")
	ec2roArn := aws.StringValue(ec2roPolicy.Policy.Arn)

	iamroPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName: aws.String(iamPolicyIAMReadOnly), PolicyDocument: aws.String(iamDocIAMReadOnly),
	})
	require.NoError(t, err, "create-policy IAMReadOnly")
	iamroArn := aws.StringValue(iamroPolicy.Policy.Arn)

	denyPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName: aws.String(iamPolicyDenyTerminate), PolicyDocument: aws.String(iamDocDenyTerminate),
	})
	require.NoError(t, err, "create-policy DenyTerminate")
	denyArn := aws.StringValue(denyPolicy.Policy.Arn)

	descAllPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName: aws.String(iamPolicyEC2DescribeAll), PolicyDocument: aws.String(iamDocEC2DescribeAll),
	})
	require.NoError(t, err, "create-policy EC2DescribeAll")
	descAllArn := aws.StringValue(descAllPolicy.Policy.Arn)

	fullAdminPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(iamPolicyFullAdmin),
		Path:           aws.String(iamPolicyFullAdminPath),
		PolicyDocument: aws.String(iamDocFullAdmin),
	})
	require.NoError(t, err, "create-policy FullAdmin")
	fullAdminArn := aws.StringValue(fullAdminPolicy.Policy.Arn)

	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String(iamUserAlice), PolicyArn: aws.String(ec2roArn)})
	require.NoError(t, err, "attach EC2ReadOnly")
	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String(iamUserAlice), PolicyArn: aws.String(iamroArn)})
	require.NoError(t, err, "attach IAMReadOnly")

	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String(iamUserBob), PolicyArn: aws.String(denyArn)})
	require.NoError(t, err, "attach DenyTerminate")

	attached, err := iamCli.ListAttachedUserPolicies(&iam.ListAttachedUserPoliciesInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "list-attached-user-policies alice")
	require.Len(t, attached.AttachedPolicies, 2, "alice attached count")

	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String(iamUserAlice), PolicyArn: aws.String(ec2roArn)})
	require.NoError(t, err, "idempotent attach")
	attached2, err := iamCli.ListAttachedUserPolicies(&iam.ListAttachedUserPoliciesInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "list-attached after idempotent attach")
	require.Len(t, attached2.AttachedPolicies, 2, "attached count must not grow on idempotent re-attach")

	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{
		UserName: aws.String(iamUserAlice), PolicyArn: aws.String(iamPolicyARN(adminAccount, "Ghost")),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String("ghost"), PolicyArn: aws.String(ec2roArn)})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// --- Enforcement ---

	_, err = charlieCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	requireAWSErrorCode(t, err, "AccessDenied")
	_, err = charlieCli.IAM.ListUsers(&iam.ListUsersInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	_, err = aliceCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "alice ec2:DescribeInstances")
	_, err = aliceCli.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{})
	require.NoError(t, err, "alice ec2:DescribeVpcs")
	_, err = aliceCli.IAM.ListUsers(&iam.ListUsersInput{})
	require.NoError(t, err, "alice iam:ListUsers")

	_, err = aliceCli.EC2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{})
	requireAWSErrorCode(t, err, "AccessDenied")
	_, err = aliceCli.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String("hack")})
	requireAWSErrorCode(t, err, "AccessDenied")

	_, err = bobCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "bob ec2:DescribeInstances")
	_, err = bobCli.EC2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{})
	require.NoError(t, err, "bob ec2:DescribeKeyPairs")
	_, err = bobCli.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: []*string{aws.String("i-fake")}})
	requireAWSErrorCode(t, err, "AccessDenied")
	_, err = bobCli.IAM.ListUsers(&iam.ListUsersInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	_, err = gw.EC2Client(t).DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "root ec2:DescribeInstances")
	_, err = iamCli.ListUsers(&iam.ListUsersInput{})
	require.NoError(t, err, "root iam:ListUsers")
	_, err = iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String("temp")})
	require.NoError(t, err, "root create-user temp")
	_, err = iamCli.DeleteUser(&iam.DeleteUserInput{UserName: aws.String("temp")})
	require.NoError(t, err, "root delete-user temp")

	_, err = iamCli.DetachUserPolicy(&iam.DetachUserPolicyInput{UserName: aws.String(iamUserAlice), PolicyArn: aws.String(ec2roArn)})
	require.NoError(t, err, "detach EC2ReadOnly")
	_, err = iamCli.DetachUserPolicy(&iam.DetachUserPolicyInput{UserName: aws.String(iamUserAlice), PolicyArn: aws.String(iamroArn)})
	require.NoError(t, err, "detach IAMReadOnly")
	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String(iamUserAlice), PolicyArn: aws.String(descAllArn)})
	require.NoError(t, err, "attach EC2DescribeAll")

	_, err = aliceCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "alice ec2:DescribeInstances (Describe*)")
	_, err = aliceCli.EC2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{})
	require.NoError(t, err, "alice ec2:DescribeKeyPairs (Describe*)")
	_, err = aliceCli.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String("x")})
	requireAWSErrorCode(t, err, "AccessDenied")
	_, err = aliceCli.IAM.ListUsers(&iam.ListUsersInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String(iamUserCharlie), PolicyArn: aws.String(fullAdminArn)})
	require.NoError(t, err, "attach FullAdmin to charlie")
	_, err = charlieCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "charlie ec2:DescribeInstances after FullAdmin")
	_, err = charlieCli.IAM.ListUsers(&iam.ListUsersInput{})
	require.NoError(t, err, "charlie iam:ListUsers after FullAdmin")
}

// TestIAMPolicyLifecycle proves that detaching alice's policy revokes her
// access, that deleting a still-attached policy surfaces DeleteConflict, and
// that after detach + delete the policy is gone (NoSuchEntity). The
// DescribeInstances allow-path needs its daemon subjects stubbed.
func TestIAMPolicyLifecycle(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)
	stubEmptyInstanceBuckets(t, gw)
	gw.StubSubject(t, "ec2.DescribeInstances", mustMarshal(t, &ec2.DescribeInstancesOutput{}))

	_, err := iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-user alice")
	_, err = iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(iamUserBob)})
	require.NoError(t, err, "create-user bob")

	key, err := iamCli.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(iamUserAlice)})
	require.NoError(t, err, "create-access-key alice")
	aliceCli := gw.ClientsWithCreds(t, aws.StringValue(key.AccessKey.AccessKeyId), aws.StringValue(key.AccessKey.SecretAccessKey))

	descAllPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName: aws.String(iamPolicyEC2DescribeAll), PolicyDocument: aws.String(iamDocEC2DescribeAll),
	})
	require.NoError(t, err, "create-policy EC2DescribeAll")
	descAllArn := aws.StringValue(descAllPolicy.Policy.Arn)
	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String(iamUserAlice), PolicyArn: aws.String(descAllArn)})
	require.NoError(t, err, "attach EC2DescribeAll to alice")

	denyPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName: aws.String(iamPolicyDenyTerminate), PolicyDocument: aws.String(iamDocDenyTerminate),
	})
	require.NoError(t, err, "create-policy DenyTerminate")
	denyArn := aws.StringValue(denyPolicy.Policy.Arn)
	_, err = iamCli.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String(iamUserBob), PolicyArn: aws.String(denyArn)})
	require.NoError(t, err, "attach DenyTerminate to bob")

	_, err = iamCli.DetachUserPolicy(&iam.DetachUserPolicyInput{UserName: aws.String(iamUserAlice), PolicyArn: aws.String(descAllArn)})
	require.NoError(t, err, "detach EC2DescribeAll")
	_, err = aliceCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	_, err = iamCli.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(denyArn)})
	requireAWSErrorCode(t, err, "DeleteConflict")

	_, err = iamCli.DetachUserPolicy(&iam.DetachUserPolicyInput{UserName: aws.String(iamUserBob), PolicyArn: aws.String(denyArn)})
	require.NoError(t, err, "detach DenyTerminate")
	_, err = iamCli.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(denyArn)})
	require.NoError(t, err, "delete-policy DenyTerminate")

	_, err = iamCli.GetPolicy(&iam.GetPolicyInput{PolicyArn: aws.String(denyArn)})
	requireAWSErrorCode(t, err, "NoSuchEntity")
}
