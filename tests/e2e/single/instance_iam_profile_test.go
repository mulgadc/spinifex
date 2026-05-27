//go:build e2e

package single

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runIAMInstanceProfileAssociation ports run-e2e.sh IAM Phase 9: EC2 IAM
// instance-profile association lifecycle. Reuses the singleton VM for the
// post-launch Associate/Replace/Disassociate cycle, then launches a single
// short-lived VM whose sole purpose is to exercise (a) RunInstances with
// --iam-instance-profile and (b) auto-disassociate on terminate.
//
// Pre-state: TestIAMRolesAndProfiles already tore everything down — this
// test recreates the role + two profiles, asserts the EC2 surface, then
// cleans up so subsequent tests see no residue.
func runIAMInstanceProfileAssociation(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — EC2 IAM Instance Profile Association")

	adminAccount := iamEnsureAdminAccountID(t, fix)
	adminPolicyARN := iamPolicyARN(adminAccount, iamPolicyAdministrator)

	// Defensive cleanup of stale state from a prior run.
	iamDeleteRoleAndProfilesBestEffort(fix, iamRoleAppName,
		[]string{iamProfileAppName, iamProfileOtherName}, adminPolicyARN)

	// Singleton instance — reused for the post-launch Associate/Replace
	// cycle. Must be running before we proceed.
	inst, _ := needInstance(t, fix)
	singletonID := aws.StringValue(inst.InstanceId)
	require.Equal(t, "running", aws.StringValue(inst.State.Name),
		"singleton must be running for IAM association lifecycle test")

	// Build role + two profiles. The second profile is the Replace target.
	harness.Step(t, "setup: create-role + 2 instance profiles")
	_, err := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(iamRoleAppName),
		AssumeRolePolicyDocument: aws.String(iamTrustPolicyEC2Standard),
	})
	require.NoError(t, err, "create-role")

	// Even though root bypasses iam:PassRole, attach AdministratorAccess
	// to match production usage and exercise the PassRole code path.
	_, err = fix.AWS.IAM.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  aws.String(iamRoleAppName),
		PolicyArn: aws.String(adminPolicyARN),
	})
	require.NoError(t, err, "attach AdministratorAccess to role")

	for _, p := range []string{iamProfileAppName, iamProfileOtherName} {
		_, err = fix.AWS.IAM.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(p),
		})
		require.NoError(t, err, "create-instance-profile %s", p)
		_, err = fix.AWS.IAM.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
			InstanceProfileName: aws.String(p),
			RoleName:            aws.String(iamRoleAppName),
		})
		require.NoError(t, err, "add-role-to-instance-profile %s", p)
	}

	// Register fixture-level cleanup. LIFO order: profile/role teardown runs
	// AFTER the ephemeral VM auto-terminates and AFTER any singleton-VM
	// disassociate this test ends with. The cleanup also tolerates partial
	// state from a mid-test failure.
	fix.Harness.RegisterCleanup(func() {
		// Disassociate anything still bound to the singleton.
		assocs, err := fix.AWS.EC2.DescribeIamInstanceProfileAssociations(
			&ec2.DescribeIamInstanceProfileAssociationsInput{
				Filters: []*ec2.Filter{{
					Name: aws.String("instance-id"), Values: []*string{aws.String(singletonID)},
				}},
			})
		if err == nil {
			for _, a := range assocs.IamInstanceProfileAssociations {
				_, _ = fix.AWS.EC2.DisassociateIamInstanceProfile(
					&ec2.DisassociateIamInstanceProfileInput{AssociationId: a.AssociationId})
			}
		}
		iamDeleteRoleAndProfilesBestEffort(fix, iamRoleAppName,
			[]string{iamProfileAppName, iamProfileOtherName}, adminPolicyARN)
	})

	// ---- Associate (post-launch) on singleton ----
	harness.Step(t, "associate-iam-instance-profile %s -> %s", iamProfileAppName, singletonID)
	assocOut, err := fix.AWS.EC2.AssociateIamInstanceProfile(&ec2.AssociateIamInstanceProfileInput{
		InstanceId:         aws.String(singletonID),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(iamProfileAppName)},
	})
	require.NoError(t, err, "associate-iam-instance-profile")
	require.NotNil(t, assocOut.IamInstanceProfileAssociation, "missing association in response")
	assocID := aws.StringValue(assocOut.IamInstanceProfileAssociation.AssociationId)
	require.True(t, strings.HasPrefix(assocID, "iip-assoc-"),
		"AssociationId %q must use iip-assoc- prefix per AWS shape", assocID)
	harness.Detail(t, "association", assocID)

	// Associate again — must surface IamInstanceProfileAlreadyAssociated.
	harness.Step(t, "associate-iam-instance-profile twice (expect IamInstanceProfileAlreadyAssociated)")
	harness.ExpectError(t, "IamInstanceProfileAlreadyAssociated", func() error {
		_, e := fix.AWS.EC2.AssociateIamInstanceProfile(&ec2.AssociateIamInstanceProfileInput{
			InstanceId:         aws.String(singletonID),
			IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(iamProfileAppName)},
		})
		return e
	})

	// DescribeInstances must surface the bound profile ARN.
	harness.Step(t, "describe-instances should expose IamInstanceProfile.Arn")
	desc, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(singletonID)},
	})
	require.NoError(t, err, "describe-instances")
	require.NotEmpty(t, desc.Reservations)
	require.NotEmpty(t, desc.Reservations[0].Instances)
	ip := desc.Reservations[0].Instances[0].IamInstanceProfile
	require.NotNil(t, ip, "DescribeInstances must populate IamInstanceProfile when bound")
	require.True(t,
		strings.HasSuffix(aws.StringValue(ip.Arn), ":instance-profile/"+iamProfileAppName),
		"unexpected IamInstanceProfile.Arn %q", aws.StringValue(ip.Arn))

	// DescribeIamInstanceProfileAssociations by id.
	harness.Step(t, "describe-iam-instance-profile-associations --association-ids %s", assocID)
	descAssoc, err := fix.AWS.EC2.DescribeIamInstanceProfileAssociations(
		&ec2.DescribeIamInstanceProfileAssociationsInput{
			AssociationIds: []*string{aws.String(assocID)},
		})
	require.NoError(t, err, "describe-iam-instance-profile-associations")
	require.Len(t, descAssoc.IamInstanceProfileAssociations, 1)
	require.Equal(t, singletonID,
		aws.StringValue(descAssoc.IamInstanceProfileAssociations[0].InstanceId))

	// DeleteInstanceProfile is refused while the profile is bound to a live
	// instance — the gateway live-instance fan-out gate from iam-roles-v1-ec2.
	harness.Step(t, "delete-instance-profile while bound (expect DeleteConflict)")
	harness.ExpectError(t, "DeleteConflict", func() error {
		_, e := fix.AWS.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
			InstanceProfileName: aws.String(iamProfileAppName),
		})
		return e
	})

	// Replace → other-profile, must mint a new assoc id.
	harness.Step(t, "replace-iam-instance-profile-association -> %s", iamProfileOtherName)
	replaceOut, err := fix.AWS.EC2.ReplaceIamInstanceProfileAssociation(
		&ec2.ReplaceIamInstanceProfileAssociationInput{
			AssociationId:      aws.String(assocID),
			IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(iamProfileOtherName)},
		})
	require.NoError(t, err, "replace-iam-instance-profile-association")
	newAssocID := aws.StringValue(replaceOut.IamInstanceProfileAssociation.AssociationId)
	require.True(t, strings.HasPrefix(newAssocID, "iip-assoc-"))
	require.NotEqual(t, assocID, newAssocID, "Replace must mint a fresh AssociationId")

	// Replace with the stale id surfaces NoSuchAssociation.
	harness.Step(t, "replace with stale id (expect NoSuchAssociation)")
	harness.ExpectError(t, "NoSuchAssociation", func() error {
		_, e := fix.AWS.EC2.ReplaceIamInstanceProfileAssociation(
			&ec2.ReplaceIamInstanceProfileAssociationInput{
				AssociationId:      aws.String(assocID),
				IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(iamProfileAppName)},
			})
		return e
	})

	// Disassociate.
	harness.Step(t, "disassociate-iam-instance-profile %s", newAssocID)
	_, err = fix.AWS.EC2.DisassociateIamInstanceProfile(&ec2.DisassociateIamInstanceProfileInput{
		AssociationId: aws.String(newAssocID),
	})
	require.NoError(t, err, "disassociate-iam-instance-profile")

	// Disassociate again with stale id.
	harness.ExpectError(t, "NoSuchAssociation", func() error {
		_, e := fix.AWS.EC2.DisassociateIamInstanceProfile(&ec2.DisassociateIamInstanceProfileInput{
			AssociationId: aws.String(newAssocID),
		})
		return e
	})

	// DescribeInstances no longer carries the profile.
	descAfter, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(singletonID)},
	})
	require.NoError(t, err, "describe-instances after disassociate")
	if ip := descAfter.Reservations[0].Instances[0].IamInstanceProfile; ip != nil {
		require.Empty(t, aws.StringValue(ip.Arn),
			"DescribeInstances must clear IamInstanceProfile.Arn after Disassociate")
	}

	// ---- RunInstances --iam-instance-profile + auto-disassociate on terminate ----
	// Reuse the singleton's launch attributes so we don't need to rediscover
	// AMI / type / key / subnet / SG — guarantees the ephemeral VM lands on a
	// known-good combination on the local single-node fixture.
	ephID := runInstanceWithProfile(t, fix, inst, iamProfileAppName)
	defer func() {
		// Belt-and-braces: even if the auto-disassociate assertion fails, we
		// must not leave a VM running. Terminate is idempotent.
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(ephID)},
		})
	}()

	// Lookup the association the daemon synthesised at launch.
	ephAssocs, err := fix.AWS.EC2.DescribeIamInstanceProfileAssociations(
		&ec2.DescribeIamInstanceProfileAssociationsInput{
			Filters: []*ec2.Filter{{
				Name: aws.String("instance-id"), Values: []*string{aws.String(ephID)},
			}},
		})
	require.NoError(t, err, "describe-iam-instance-profile-associations --instance-id %s", ephID)
	require.Len(t, ephAssocs.IamInstanceProfileAssociations, 1,
		"RunInstances --iam-instance-profile must auto-create an association")
	ephAssocID := aws.StringValue(ephAssocs.IamInstanceProfileAssociations[0].AssociationId)
	require.True(t, strings.HasPrefix(ephAssocID, "iip-assoc-"))

	// DescribeInstances on the ephemeral VM surfaces the profile.
	ephDesc, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(ephID)},
	})
	require.NoError(t, err, "describe-instances ephemeral")
	require.NotEmpty(t, ephDesc.Reservations)
	require.NotEmpty(t, ephDesc.Reservations[0].Instances)
	require.NotNil(t, ephDesc.Reservations[0].Instances[0].IamInstanceProfile,
		"RunInstances --iam-instance-profile must be visible on DescribeInstances")

	// Terminate → wait for auto-disassociate.
	harness.Step(t, "terminate ephemeral %s (auto-disassociate)", ephID)
	_, err = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(ephID)},
	})
	require.NoError(t, err, "terminate-instances ephemeral")
	harness.WaitForInstanceState(t, fix.AWS, ephID, "terminated")

	// Allow a brief window for the daemon's auto-disassociate to drain to KV.
	// The disassociate is synchronous on the same handler that flips state, so
	// the wait above is usually sufficient — bound a short retry just in case.
	require.Eventually(t, func() bool {
		_, e := fix.AWS.EC2.DisassociateIamInstanceProfile(&ec2.DisassociateIamInstanceProfileInput{
			AssociationId: aws.String(ephAssocID),
		})
		return harness.ErrorCodeIs(e, "NoSuchAssociation")
	}, 30*time.Second, 1*time.Second,
		"terminated instance %s must auto-disassociate %s", ephID, ephAssocID)
}

// runInstanceWithProfile launches a single short-lived VM mirroring src's
// launch attributes, with the named instance profile attached. Polls to
// running; returns the new instance ID. Caller owns terminate.
func runInstanceWithProfile(t *testing.T, fix *Fixture, src *ec2.Instance, profileName string) string {
	t.Helper()
	require.NotEmpty(t, src.SecurityGroups, "source instance has no security groups")
	out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          src.ImageId,
		InstanceType:     src.InstanceType,
		KeyName:          src.KeyName,
		SubnetId:         src.SubnetId,
		SecurityGroupIds: []*string{src.SecurityGroups[0].GroupId},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
			Name: aws.String(profileName),
		},
	})
	require.NoError(t, err, "run-instances --iam-instance-profile")
	require.NotEmpty(t, out.Instances, "RunInstances returned 0 instances")
	id := aws.StringValue(out.Instances[0].InstanceId)
	require.True(t, strings.HasPrefix(id, "i-"), "unexpected InstanceId %q", id)
	harness.WaitForInstanceState(t, fix.AWS, id, "running")
	return id
}
