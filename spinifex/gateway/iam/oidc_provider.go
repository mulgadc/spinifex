package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

func CreateOpenIDConnectProvider(accountID string, input *iam.CreateOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.CreateOpenIDConnectProviderOutput, error) {
	if input.Url == nil || *input.Url == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateOpenIDConnectProvider(accountID, input)
}

func GetOpenIDConnectProvider(accountID string, input *iam.GetOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.GetOpenIDConnectProviderOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetOpenIDConnectProvider(accountID, input)
}

func ListOpenIDConnectProviders(accountID string, input *iam.ListOpenIDConnectProvidersInput, svc handlers_iam.IAMService) (*iam.ListOpenIDConnectProvidersOutput, error) {
	return svc.ListOpenIDConnectProviders(accountID, input)
}

func DeleteOpenIDConnectProvider(accountID string, input *iam.DeleteOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteOpenIDConnectProvider(accountID, input)
}

func TagOpenIDConnectProvider(accountID string, input *iam.TagOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.TagOpenIDConnectProviderOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.TagOpenIDConnectProvider(accountID, input)
}

func UntagOpenIDConnectProvider(accountID string, input *iam.UntagOpenIDConnectProviderInput, svc handlers_iam.IAMService) (*iam.UntagOpenIDConnectProviderOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UntagOpenIDConnectProvider(accountID, input)
}

func ListOpenIDConnectProviderTags(accountID string, input *iam.ListOpenIDConnectProviderTagsInput, svc handlers_iam.IAMService) (*iam.ListOpenIDConnectProviderTagsOutput, error) {
	if input.OpenIDConnectProviderArn == nil || *input.OpenIDConnectProviderArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListOpenIDConnectProviderTags(accountID, input)
}
