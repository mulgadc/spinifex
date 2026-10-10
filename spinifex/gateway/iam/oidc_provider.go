package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// CreateOpenIDConnectProvider implements the IAM CreateOpenIDConnectProvider action, registering
// an OIDC identity provider. It returns MissingParameter when Url is absent, then calls the IAM
// service for accountID.
func CreateOpenIDConnectProvider(accountID string, input *iam.CreateOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.CreateOpenIDConnectProviderOutput, error) {
	if input.Url == nil || *input.Url == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateOpenIDConnectProvider(accountID, input)
}

// GetOpenIDConnectProvider implements the IAM GetOpenIDConnectProvider action, describing an OIDC
// provider. It returns MissingParameter when OpenIDConnectProviderArn is absent, then calls the
// IAM service for accountID.
func GetOpenIDConnectProvider(accountID string, input *iam.GetOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.GetOpenIDConnectProviderOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetOpenIDConnectProvider(accountID, input)
}

// ListOpenIDConnectProviders implements the IAM ListOpenIDConnectProviders action. The action is
// unpaginated in AWS, so the service result is returned as is.
func ListOpenIDConnectProviders(accountID string, input *iam.ListOpenIDConnectProvidersInput, svc handlers_iam.IAMService) (*iam.ListOpenIDConnectProvidersOutput, error) {
	return svc.ListOpenIDConnectProviders(accountID, input)
}

// DeleteOpenIDConnectProvider implements the IAM DeleteOpenIDConnectProvider action, deleting an
// OIDC provider. It returns MissingParameter when OpenIDConnectProviderArn is absent, then calls
// the IAM service for accountID.
func DeleteOpenIDConnectProvider(accountID string, input *iam.DeleteOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteOpenIDConnectProvider(accountID, input)
}

// TagOpenIDConnectProvider implements the IAM TagOpenIDConnectProvider action. It returns
// MissingParameter when OpenIDConnectProviderArn is absent or Tags is empty.
func TagOpenIDConnectProvider(accountID string, input *iam.TagOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.TagOpenIDConnectProviderOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.TagOpenIDConnectProvider(accountID, input)
}

// UntagOpenIDConnectProvider implements the IAM UntagOpenIDConnectProvider action. It returns
// MissingParameter when OpenIDConnectProviderArn is absent or TagKeys is empty.
func UntagOpenIDConnectProvider(accountID string, input *iam.UntagOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.UntagOpenIDConnectProviderOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UntagOpenIDConnectProvider(accountID, input)
}

// ListOpenIDConnectProviderTags implements the IAM ListOpenIDConnectProviderTags action, listing
// an OIDC provider's tags. It requires OpenIDConnectProviderArn; results are paged here by Marker
// and MaxItems.
func ListOpenIDConnectProviderTags(accountID string, input *iam.ListOpenIDConnectProviderTagsInput, svc handlers_iam.IAMService) (*iam.ListOpenIDConnectProviderTagsOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListOpenIDConnectProviderTags(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Tags, out.Marker = paginate(p, out.Tags, tagKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}
