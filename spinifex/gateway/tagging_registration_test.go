package gateway

import (
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	taggingawsapi "github.com/mulgadc/spinifex/spinifex/domains/tagging/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// withTagging registers the Resource Groups Tagging API on gw backed by agg, as
// the awsgw role composes it.
func withTagging(gw *GatewayConfig, agg *tagging.Aggregator) *GatewayConfig {
	b := dispatch.NewBuilder()
	if err := b.Register(taggingawsapi.NewRegistration(taggingawsapi.Deps{Aggregator: agg})); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}
