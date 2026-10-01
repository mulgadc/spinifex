package gateway

import (
	"github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
)

// ecrRepositoryEndpoint adapts gateway composition settings to the ECR domain
// action adapter. AWS-visible repository projection remains domain ownership.
func (gw *GatewayConfig) ecrRepositoryEndpoint() awsapi.RepositoryEndpoint {
	return awsapi.RepositoryEndpoint{
		Region:         gw.Region,
		ServicesDomain: gw.InternalSuffix,
		RegistryHost:   gw.RegistryHost,
		RegistryPort:   gw.RegistryPort,
	}
}
