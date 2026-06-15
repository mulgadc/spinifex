package gateway_ec2_instance

import "github.com/aws/aws-sdk-go/service/iam"

// OIDC identity-provider registry stubs so fakeIAMService satisfies the
// IAMService interface; EC2 instance-profile paths never call these.

func (f *fakeIAMService) CreateOpenIDConnectProvider(string, *iam.CreateOpenIDConnectProviderInput) (*iam.CreateOpenIDConnectProviderOutput, error) {
	return nil, nil
}
func (f *fakeIAMService) GetOpenIDConnectProvider(string, *iam.GetOpenIDConnectProviderInput) (*iam.GetOpenIDConnectProviderOutput, error) {
	return nil, nil
}
func (f *fakeIAMService) ListOpenIDConnectProviders(string, *iam.ListOpenIDConnectProvidersInput) (*iam.ListOpenIDConnectProvidersOutput, error) {
	return nil, nil
}
func (f *fakeIAMService) DeleteOpenIDConnectProvider(string, *iam.DeleteOpenIDConnectProviderInput) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	return nil, nil
}
