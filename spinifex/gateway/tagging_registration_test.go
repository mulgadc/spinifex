package gateway

import "github.com/mulgadc/spinifex/spinifex/domains/tagging"

// withTagging wires the Resource Groups Tagging API onto gw backed by agg, as
// the awsgw role composes it.
func withTagging(gw *GatewayConfig, agg *tagging.Aggregator) *GatewayConfig {
	gw.TaggingResources = agg
	return gw
}
