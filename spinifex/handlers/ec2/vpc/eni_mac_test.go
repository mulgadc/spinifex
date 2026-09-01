package handlers_ec2_vpc

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateENIMac_Deterministic(t *testing.T) {
	t.Parallel()
	mac1 := generateENIMac("eni-abc123")
	mac2 := generateENIMac("eni-abc123")
	assert.Equal(t, mac1, mac2, "same input must produce same MAC")
}

func TestGenerateENIMac_DifferentInputs(t *testing.T) {
	t.Parallel()
	mac1 := generateENIMac("eni-aaa")
	mac2 := generateENIMac("eni-bbb")
	assert.NotEqual(t, mac1, mac2, "different inputs should produce different MACs")
}

func TestGenerateENIMac_LocallyAdministered(t *testing.T) {
	t.Parallel()
	mac := generateENIMac("eni-test123")
	hw, err := net.ParseMAC(mac)
	require.NoError(t, err)
	// IEEE 802 reserved bits on first octet: bit0=0 unicast, bit1=1 LAA.
	assert.Equal(t, byte(0x02), hw[0]&0x03)
}

func TestGenerateENIMac_EmptyString(t *testing.T) {
	t.Parallel()
	mac := generateENIMac("")
	hw, err := net.ParseMAC(mac)
	require.NoError(t, err)
	assert.Equal(t, byte(0x02), hw[0]&0x03)
	// New impl hashes the prefix-and-id; empty input no longer collapses to
	// the degenerate 02:00:00:00:00:00 of the old 24-bit impl.
	assert.NotEqual(t, "02:00:00:00:00:00", hw.String())
}
