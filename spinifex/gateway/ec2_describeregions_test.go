package gateway_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/stretchr/testify/assert"
)

// TestAdvertisedEndpoint_ResolvesFromRegistryHost asserts DescribeRegions'
// endpoint tracks this gateway's own reachable host:port (the same
// RegistryHost/RegistryPort precedence the ECR repository endpoint also
// applies),
// rather than a literal that never changes with deployment.
func TestAdvertisedEndpoint_ResolvesFromRegistryHost(t *testing.T) {
	cases := []struct {
		name         string
		registryHost string
		port         string
		want         string
	}{
		{"no concrete host falls back to localhost:9999", "", "", "https://localhost:9999"},
		{"concrete host, default port", "10.0.0.5", "", "https://10.0.0.5:9999"},
		{"concrete host, explicit port", "10.0.0.5", "9999", "https://10.0.0.5:9999"},
		{"concrete host, 443 omits port", "gw.example.com", "443", "https://gw.example.com"},
		{"concrete host, custom port", "gw.example.com", "8443", "https://gw.example.com:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &gateway.GatewayConfig{RegistryHost: tc.registryHost, RegistryPort: tc.port}
			assert.Equal(t, tc.want, gw.AdvertisedEndpoint())
		})
	}
}
