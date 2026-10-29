package gateway

// withRDS wires RDS onto gw as the awsgw role composes it.
func withRDS(gw *GatewayConfig) *GatewayConfig {
	return gw
}
