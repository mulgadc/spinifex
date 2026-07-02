package handlers_iam

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// EC2InstanceTrustPolicy is the trust document AssumeRoleForInstance requires:
// the EC2 service principal may assume the role so IMDS can mint instance-role
// credentials for the VM.
const EC2InstanceTrustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

// SystemInstanceRoleEnsurer is the narrow IAM surface needed to find-or-create a
// Spinifex-managed instance role and the instance profile that fronts it.
type SystemInstanceRoleEnsurer interface {
	GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error)
	CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error)
	PutRolePolicy(accountID string, input *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error)
	GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error)
	CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error)
	AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error)
}

// EnsureSystemInstanceProfile find-or-creates a system-managed role (EC2 trust +
// the given inline permission policy) plus an instance profile carrying it,
// returning the profile ARN to attach at launch so IMDS serves the role.
// Idempotent: a re-launch re-asserts the policy, and a racing create converges
// via the EntityAlreadyExists re-read. The role/profile share a name.
func EnsureSystemInstanceProfile(e SystemInstanceRoleEnsurer, accountID, roleName, policyName, policyDoc string) (string, error) {
	if err := ensureSystemRole(e, accountID, roleName); err != nil {
		return "", err
	}
	if _, err := e.PutRolePolicy(accountID, &iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(policyDoc),
	}); err != nil {
		return "", fmt.Errorf("put role policy %q on %q: %w", policyName, roleName, err)
	}
	return ensureSystemInstanceProfile(e, accountID, roleName)
}

// ensureSystemRole creates the role with the EC2 trust policy when absent. A
// racing create that lost (EntityAlreadyExists) is tolerated as success.
func ensureSystemRole(e SystemInstanceRoleEnsurer, accountID, roleName string) error {
	if _, err := e.GetRole(accountID, &iam.GetRoleInput{RoleName: aws.String(roleName)}); err == nil {
		return nil
	} else if err.Error() != awserrors.ErrorIAMNoSuchEntity {
		return fmt.Errorf("get role %q: %w", roleName, err)
	}

	_, err := e.CreateRole(accountID, &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(EC2InstanceTrustPolicy),
		Description:              aws.String("Spinifex-managed system instance role"),
	})
	if err == nil || err.Error() == awserrors.ErrorIAMEntityAlreadyExists {
		return nil
	}
	return fmt.Errorf("create role %q: %w", roleName, err)
}

// ensureSystemInstanceProfile guarantees an instance profile named for the role
// exists and carries it, returning the profile ARN. Concurrent launches converge
// on the same profile (EntityAlreadyExists / LimitExceeded treated as success).
func ensureSystemInstanceProfile(e SystemInstanceRoleEnsurer, accountID, roleName string) (string, error) {
	out, err := e.GetInstanceProfile(accountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
	})
	if err == nil {
		return attachSystemRole(e, accountID, roleName,
			aws.StringValue(out.InstanceProfile.Arn), len(out.InstanceProfile.Roles) > 0)
	}
	if err.Error() != awserrors.ErrorIAMNoSuchEntity {
		return "", fmt.Errorf("get instance profile %q: %w", roleName, err)
	}

	created, err := e.CreateInstanceProfile(accountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
	})
	if err != nil {
		if err.Error() != awserrors.ErrorIAMEntityAlreadyExists {
			return "", fmt.Errorf("create instance profile %q: %w", roleName, err)
		}
		got, gerr := e.GetInstanceProfile(accountID, &iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String(roleName),
		})
		if gerr != nil {
			return "", fmt.Errorf("re-get instance profile %q: %w", roleName, gerr)
		}
		return attachSystemRole(e, accountID, roleName,
			aws.StringValue(got.InstanceProfile.Arn), len(got.InstanceProfile.Roles) > 0)
	}
	return attachSystemRole(e, accountID, roleName,
		aws.StringValue(created.InstanceProfile.Arn), false)
}

// attachSystemRole adds roleName to the profile unless already attached. A
// LimitExceeded error means a racing launch attached it first — treated success.
func attachSystemRole(e SystemInstanceRoleEnsurer, accountID, roleName, profileARN string, alreadyAttached bool) (string, error) {
	if alreadyAttached {
		return profileARN, nil
	}
	_, err := e.AddRoleToInstanceProfile(accountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
		RoleName:            aws.String(roleName),
	})
	if err != nil && err.Error() != awserrors.ErrorIAMLimitExceeded {
		return "", fmt.Errorf("add role %q to instance profile %q: %w", roleName, roleName, err)
	}
	return profileARN, nil
}
