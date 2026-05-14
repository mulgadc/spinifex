package vpcd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Additional edge-case tests for subnetGateway.
// Core happy-path tests are in topology_test.go.

func TestSubnetGateway_Slash28(t *testing.T) {
	gw, prefix, err := subnetGateway("172.31.0.0/28")
	require.NoError(t, err)
	assert.Equal(t, "172.31.0.1", gw)
	assert.Equal(t, 28, prefix)
}

func TestSubnetGateway_IPv6(t *testing.T) {
	_, _, err := subnetGateway("2001:db8::/32")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only IPv4 supported")
}
