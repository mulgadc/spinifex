package gateway

import (
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
)

// ecrRepositoryEndpoint adapts gateway composition settings to the ECR token
// action, which still constructs its canonical caller principal in gateway.
// Repository metadata actions receive this profile from their composed service.
func (gw *GatewayConfig) ecrRepositoryEndpoint() awsapi.RepositoryEndpoint {
	return awsapi.RepositoryEndpoint{
		Region:         gw.Region,
		ServicesDomain: gw.InternalSuffix,
		RegistryHost:   gw.RegistryHost,
		RegistryPort:   gw.RegistryPort,
	}
}
