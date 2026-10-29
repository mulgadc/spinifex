package gateway

import "net/http"

// withELBv2 wires ELBv2 onto gw as the awsgw role composes it.
func withELBv2(gw *GatewayConfig) *GatewayConfig {
	return gw
}

// serveELBv2 dispatches r to the ELBv2 service exactly as Request does past
// its NATS gate.
func (gw *GatewayConfig) serveELBv2(w http.ResponseWriter, r *http.Request) error {
	return gw.ELBv2_Request(w, r)
}

// elbv2ServedActions lists every action the ELBv2 service dispatches.
func elbv2ServedActions() []string {
	return mapKeys(elbv2Actions)
}
