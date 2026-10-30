//test:in-package — drives the unexported action table without a request.

package awsapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DescribeRegions must advertise the endpoint the handler was composed with, so
// a caller on a concrete-host deployment is never told to dial its own loopback.
func TestDescribeRegions_UsesAdvertisedEndpoint(t *testing.T) {
	c := &call{Handler: NewHandler(Deps{Endpoint: "https://10.0.0.5:9999"}), Region: "ap-southeast-2"}
	h := ec2Actions["DescribeRegions"]
	input, err := h.parse(map[string]string{})
	require.NoError(t, err)
	xmlOutput, err := h.dispatch(t.Context(), "DescribeRegions", input, c, "acct-123")
	require.NoError(t, err)

	body := string(xmlOutput)
	assert.Contains(t, body, "<regionEndpoint>https://10.0.0.5:9999</regionEndpoint>")
	assert.NotContains(t, body, "localhost")
}
