package handlers_iam

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// AdminUserName is the IAM user every provisioned account gets, holding the
// account's AdministratorAccess policy. Callers hand its access key to the
// account owner as their first credential.
const AdminUserName = "admin"

// AdminPolicyName is the managed policy attached to AdminUserName.
const AdminPolicyName = "AdministratorAccess"

// adminAccessPolicyDocument grants every action on every resource within the
// account. Account scoping comes from the policy living in the account, not
// from the document.
const adminAccessPolicyDocument = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`

// ProvisionedAccount is the credential set handed back to whoever asked for an
// account: enough to sign a request, and nothing else.
type ProvisionedAccount struct {
	AccountID       string
	AccountName     string
	AdminUser       string
	AccessKeyID     string
	SecretAccessKey string
}

// NormalizeAccountName is the canonical form used for comparison and for the
// name-reservation key. Names arrive from a web form, so case and stray
// whitespace must not create a second account for the same person.
func NormalizeAccountName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// ProvisionAccount builds out an already-created account: the admin user, the
// AdministratorAccess policy, the attachment, and one access key.
//
// Every step is create-if-absent, so calling it again against a partially
// provisioned account finishes the job rather than failing. That is what makes
// a retry safe: there is no rollback for account creation, so the recovery path
// is to resume. The access key is the exception — it cannot be re-read — so any
// key left by an earlier attempt is deleted and replaced, leaving exactly one.
func ProvisionAccount(svc IAMService, accountID, name string) (*ProvisionedAccount, error) {
	if svc == nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if accountID == "" || name == "" {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	if _, err := svc.CreateUser(accountID, &iam.CreateUserInput{
		UserName: aws.String(AdminUserName),
	}); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("create admin user: %w", err)
	}

	if _, err := svc.CreatePolicy(accountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String(AdminPolicyName),
		PolicyDocument: aws.String(adminAccessPolicyDocument),
	}); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("create admin policy: %w", err)
	}

	policyARN := arn.FormatIAMPath(arn.IAMPolicy, accountID, "/", AdminPolicyName)
	if _, err := svc.AttachUserPolicy(accountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String(AdminUserName),
		PolicyArn: aws.String(policyARN),
	}); err != nil {
		return nil, fmt.Errorf("attach admin policy: %w", err)
	}

	if err := deleteAccessKeys(svc, accountID); err != nil {
		return nil, err
	}

	akOut, err := svc.CreateAccessKey(accountID, &iam.CreateAccessKeyInput{
		UserName: aws.String(AdminUserName),
	})
	if err != nil {
		return nil, fmt.Errorf("create access key: %w", err)
	}
	if akOut == nil || akOut.AccessKey == nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	slog.Info("Account provisioned", "accountID", accountID, "adminUser", AdminUserName)

	return &ProvisionedAccount{
		AccountID:       accountID,
		AccountName:     name,
		AdminUser:       AdminUserName,
		AccessKeyID:     aws.StringValue(akOut.AccessKey.AccessKeyId),
		SecretAccessKey: aws.StringValue(akOut.AccessKey.SecretAccessKey),
	}, nil
}

// deleteAccessKeys removes every existing key for the admin user. A key from an
// abandoned attempt was never returned to anyone, so it is unusable by its
// intended owner but still authenticates — leaving it would grant standing
// access to whoever could read that attempt's logs.
func deleteAccessKeys(svc IAMService, accountID string) error {
	listed, err := svc.ListAccessKeys(accountID, &iam.ListAccessKeysInput{
		UserName: aws.String(AdminUserName),
	})
	if err != nil {
		return fmt.Errorf("list access keys: %w", err)
	}
	if listed == nil {
		return nil
	}

	for _, meta := range listed.AccessKeyMetadata {
		if meta == nil || meta.AccessKeyId == nil {
			continue
		}
		if _, err := svc.DeleteAccessKey(accountID, &iam.DeleteAccessKeyInput{
			UserName:    aws.String(AdminUserName),
			AccessKeyId: meta.AccessKeyId,
		}); err != nil {
			return fmt.Errorf("delete stale access key: %w", err)
		}
		slog.Warn("Removed access key left by an earlier provisioning attempt",
			"accountID", accountID, "accessKeyID", aws.StringValue(meta.AccessKeyId))
	}
	return nil
}

// isAlreadyExists reports whether err is the IAM duplicate-entity error, which
// a resumed provisioning run treats as the step having already succeeded.
func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), awserrors.ErrorIAMEntityAlreadyExists)
}
