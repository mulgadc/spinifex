package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdvertisedEndpoint_ResolvesFromRegistryHost asserts DescribeRegions'
// endpoint tracks this gateway's own reachable host:port (the same
// RegistryHost/RegistryPort precedence ecrRegistryHost already applies),
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
			gw := &GatewayConfig{RegistryHost: tc.registryHost, RegistryPort: tc.port}
			assert.Equal(t, tc.want, gw.advertisedEndpoint())
		})
	}
}

// TestDescribeRegions_UsesAdvertisedEndpoint drives the real dispatch entry
// EC2_Request uses and asserts the resolved gateway address reaches the wire
// response. A caller on a concrete-host deployment must never be told to
// dial its own loopback, which is the defect this guards against.
func TestDescribeRegions_UsesAdvertisedEndpoint(t *testing.T) {
	h := ec2Actions["DescribeRegions"]
	input, err := h.parse(map[string]string{})
	require.NoError(t, err)

	gw := &GatewayConfig{Region: "ap-southeast-2", RegistryHost: "10.0.0.5", RegistryPort: "9999"}
	xmlOutput, err := h.dispatch("DescribeRegions", input, gw, "acct-123", nil)
	require.NoError(t, err)

	body := string(xmlOutput)
	assert.Contains(t, body, "<regionEndpoint>https://10.0.0.5:9999</regionEndpoint>")
	assert.NotContains(t, body, "localhost", "must not advertise the gateway's own loopback")
}
