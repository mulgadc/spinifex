package gateway_sts

import "github.com/aws/aws-sdk-go/service/iam"

// OIDC identity-provider registry stubs so stubIAMService satisfies the
// IAMService interface; STS gateway tests never call these.

func (s *stubIAMService) CreateOpenIDConnectProvider(string, *iam.CreateOpenIDConnectProviderInput) (*iam.CreateOpenIDConnectProviderOutput, error) {
	return nil, nil
}
func (s *stubIAMService) GetOpenIDConnectProvider(string, *iam.GetOpenIDConnectProviderInput) (*iam.GetOpenIDConnectProviderOutput, error) {
	return nil, nil
}
func (s *stubIAMService) ListOpenIDConnectProviders(string, *iam.ListOpenIDConnectProvidersInput) (*iam.ListOpenIDConnectProvidersOutput, error) {
	return nil, nil
}
func (s *stubIAMService) DeleteOpenIDConnectProvider(string, *iam.DeleteOpenIDConnectProviderInput) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	return nil, nil
}
