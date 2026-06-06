package gateway

import "github.com/aws/aws-sdk-go/service/iam"

// OIDC identity-provider registry methods — IRSA stubs so the mock IAM
// services satisfy the IAMService interface. No gateway test exercises these
// paths directly (covered by handlers/iam unit tests).

func (m *mockIAMService) CreateOpenIDConnectProvider(string, *iam.CreateOpenIDConnectProviderInput) (*iam.CreateOpenIDConnectProviderOutput, error) {
	return nil, nil
}
func (m *mockIAMService) GetOpenIDConnectProvider(string, *iam.GetOpenIDConnectProviderInput) (*iam.GetOpenIDConnectProviderOutput, error) {
	return nil, nil
}
func (m *mockIAMService) ListOpenIDConnectProviders(string, *iam.ListOpenIDConnectProvidersInput) (*iam.ListOpenIDConnectProvidersOutput, error) {
	return nil, nil
}
func (m *mockIAMService) DeleteOpenIDConnectProvider(string, *iam.DeleteOpenIDConnectProviderInput) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	return nil, nil
}

func (m *flexMockIAMService) CreateOpenIDConnectProvider(string, *iam.CreateOpenIDConnectProviderInput) (*iam.CreateOpenIDConnectProviderOutput, error) {
	return nil, nil
}
func (m *flexMockIAMService) GetOpenIDConnectProvider(string, *iam.GetOpenIDConnectProviderInput) (*iam.GetOpenIDConnectProviderOutput, error) {
	return nil, nil
}
func (m *flexMockIAMService) ListOpenIDConnectProviders(string, *iam.ListOpenIDConnectProvidersInput) (*iam.ListOpenIDConnectProvidersOutput, error) {
	return nil, nil
}
func (m *flexMockIAMService) DeleteOpenIDConnectProvider(string, *iam.DeleteOpenIDConnectProviderInput) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	return nil, nil
}
