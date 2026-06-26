package handlers_ecs

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	// ecsInstanceRoleName is the role + instance-profile name AWS uses for ECS
	// container instances. The agent draws its creds from this role over IMDS.
	ecsInstanceRoleName = "ecsInstanceRole"

	// ecsInstanceRolePolicyName is the customer-managed policy granting ecs:* so
	// the gateway's assumed-role ECS authz (checkPolicy) admits the agent.
	ecsInstanceRolePolicyName = "ecsInstanceRolePolicy"

	// ecsAssumeRolePolicy is the EC2 trust the STS AssumeRoleForInstance path
	// whitelists; it must match exactly for IMDS role-cred vending to admit it.
	ecsAssumeRolePolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

	// ecsInstanceRolePolicyDoc grants the container-instance agent the ECS
	// control-plane actions the gateway enforces for assumed-role principals.
	ecsInstanceRolePolicyDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ecs:*","Resource":"*"}]}`
)

// ensureECSInstanceProfile find-or-creates the ecsInstanceRole, its ecs:* policy
// attachment, and the matching instance profile, returning the profile ARN.
// Every step is idempotent so concurrent ProvisionCapacity calls converge.
func (s *Service) ensureECSInstanceProfile(accountID string) (string, error) {
	if err := s.ensureECSRole(accountID); err != nil {
		return "", err
	}
	if err := s.ensureECSRolePolicy(accountID); err != nil {
		return "", err
	}
	return s.ensureECSInstanceProfileBinding(accountID)
}

// ensureECSRole find-or-creates ecsInstanceRole with the EC2 trust policy,
// converging on a racing creator via re-GetRole.
func (s *Service) ensureECSRole(accountID string) error {
	_, err := s.deps.IAM.GetRole(accountID, &iam.GetRoleInput{RoleName: aws.String(ecsInstanceRoleName)})
	if err == nil {
		return nil
	}
	if err.Error() != awserrors.ErrorIAMNoSuchEntity {
		return fmt.Errorf("get role %q: %w", ecsInstanceRoleName, err)
	}

	_, err = s.deps.IAM.CreateRole(accountID, &iam.CreateRoleInput{
		RoleName:                 aws.String(ecsInstanceRoleName),
		AssumeRolePolicyDocument: aws.String(ecsAssumeRolePolicy),
	})
	if err == nil {
		return nil
	}
	if err.Error() != awserrors.ErrorIAMEntityAlreadyExists {
		return fmt.Errorf("create role %q: %w", ecsInstanceRoleName, err)
	}
	// A racing call created it first; re-read to converge.
	if _, gerr := s.deps.IAM.GetRole(accountID, &iam.GetRoleInput{RoleName: aws.String(ecsInstanceRoleName)}); gerr != nil {
		return fmt.Errorf("re-get role %q: %w", ecsInstanceRoleName, gerr)
	}
	return nil
}

// ensureECSRolePolicy creates the ecs:* policy (resolving an existing one on a
// race) and attaches it to the role; an already-attached policy is success.
func (s *Service) ensureECSRolePolicy(accountID string) error {
	policyARN := fmt.Sprintf("arn:aws:iam::%s:policy/%s", accountID, ecsInstanceRolePolicyName)

	out, err := s.deps.IAM.CreatePolicy(accountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String(ecsInstanceRolePolicyName),
		PolicyDocument: aws.String(ecsInstanceRolePolicyDoc),
	})
	switch {
	case err == nil:
		if out != nil && out.Policy != nil {
			policyARN = aws.StringValue(out.Policy.Arn)
		}
	case err.Error() == awserrors.ErrorIAMEntityAlreadyExists:
		// Policy already exists; the account ARN form is deterministic.
	default:
		return fmt.Errorf("create policy %q: %w", ecsInstanceRolePolicyName, err)
	}

	_, err = s.deps.IAM.AttachRolePolicy(accountID, &iam.AttachRolePolicyInput{
		RoleName:  aws.String(ecsInstanceRoleName),
		PolicyArn: aws.String(policyARN),
	})
	if err != nil && err.Error() != awserrors.ErrorIAMEntityAlreadyExists {
		return fmt.Errorf("attach policy %q to role %q: %w", policyARN, ecsInstanceRoleName, err)
	}
	return nil
}

// ensureECSInstanceProfileBinding guarantees the ecsInstanceRole instance
// profile exists and carries the role, returning its ARN. Mirrors the EKS
// node-profile ensure path; idempotent under concurrent launches.
func (s *Service) ensureECSInstanceProfileBinding(accountID string) (string, error) {
	out, err := s.deps.IAM.GetInstanceProfile(accountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(ecsInstanceRoleName),
	})
	if err == nil {
		return s.attachECSRoleToProfile(accountID, aws.StringValue(out.InstanceProfile.Arn), len(out.InstanceProfile.Roles) > 0)
	}
	if err.Error() != awserrors.ErrorIAMNoSuchEntity {
		return "", fmt.Errorf("get instance profile %q: %w", ecsInstanceRoleName, err)
	}

	created, err := s.deps.IAM.CreateInstanceProfile(accountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(ecsInstanceRoleName),
	})
	if err != nil {
		if err.Error() != awserrors.ErrorIAMEntityAlreadyExists {
			return "", fmt.Errorf("create instance profile %q: %w", ecsInstanceRoleName, err)
		}
		got, gerr := s.deps.IAM.GetInstanceProfile(accountID, &iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String(ecsInstanceRoleName),
		})
		if gerr != nil {
			return "", fmt.Errorf("re-get instance profile %q: %w", ecsInstanceRoleName, gerr)
		}
		return s.attachECSRoleToProfile(accountID, aws.StringValue(got.InstanceProfile.Arn), len(got.InstanceProfile.Roles) > 0)
	}
	return s.attachECSRoleToProfile(accountID, aws.StringValue(created.InstanceProfile.Arn), false)
}

// attachECSRoleToProfile adds the role to the profile unless already attached,
// returning the profile ARN. A LimitExceeded error means a racing launch
// attached it first and is treated as success.
func (s *Service) attachECSRoleToProfile(accountID, profileARN string, alreadyAttached bool) (string, error) {
	if alreadyAttached {
		return profileARN, nil
	}
	_, err := s.deps.IAM.AddRoleToInstanceProfile(accountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(ecsInstanceRoleName),
		RoleName:            aws.String(ecsInstanceRoleName),
	})
	if err != nil && err.Error() != awserrors.ErrorIAMLimitExceeded {
		return "", fmt.Errorf("add role to instance profile %q: %w", ecsInstanceRoleName, err)
	}
	return profileARN, nil
}
