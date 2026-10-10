package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// CreateAccessKey implements the IAM CreateAccessKey action, issuing a new key pair for UserName
// (MissingParameter when absent). The output is the only place the secret is returned.
func CreateAccessKey(accountID string, input *iam.CreateAccessKeyInput, svc handlers_iam.IAMService) (*iam.CreateAccessKeyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateAccessKey(accountID, input)
}

// ListAccessKeys implements the IAM ListAccessKeys action, listing a user's access key metadata.
// It requires UserName; results are paged here by Marker and MaxItems.
func ListAccessKeys(accountID string, input *iam.ListAccessKeysInput, svc handlers_iam.IAMService) (*iam.ListAccessKeysOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListAccessKeys(accountID, input)
	if err != nil {
		return nil, err
	}
	out.AccessKeyMetadata, out.Marker = paginate(p, out.AccessKeyMetadata, accessKeyKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// DeleteAccessKey implements the IAM DeleteAccessKey action, deleting a user's access key. It
// returns MissingParameter when UserName or AccessKeyId is absent, then calls the IAM service for
// accountID.
func DeleteAccessKey(accountID string, input *iam.DeleteAccessKeyInput, svc handlers_iam.IAMService) (*iam.DeleteAccessKeyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.AccessKeyId == nil || *input.AccessKeyId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteAccessKey(accountID, input)
}

// UpdateAccessKey implements the IAM UpdateAccessKey action, setting an access key Active or
// Inactive. It returns MissingParameter when AccessKeyId or Status is absent, then calls the IAM
// service for accountID.
func UpdateAccessKey(accountID string, input *iam.UpdateAccessKeyInput, svc handlers_iam.IAMService) (*iam.UpdateAccessKeyOutput, error) {
	if input.AccessKeyId == nil || *input.AccessKeyId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.Status == nil || *input.Status == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UpdateAccessKey(accountID, input)
}
