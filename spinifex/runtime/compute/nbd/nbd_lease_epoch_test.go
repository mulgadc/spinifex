package nbd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildArgs_LeaseEpochOnlyWhenSet(t *testing.T) {
	cfg := NBDKitConfig{Socket: "/run/x.sock", PluginPath: "/p.so", Volume: "vol-1", Size: 1}
	args, err := cfg.buildArgs()
	require.NoError(t, err)
	for _, a := range args {
		assert.NotContains(t, a, "lease_epoch", "an unset epoch is omitted for older plugins")
	}
	cfg.LeaseEpoch = 4242
	args, err = cfg.buildArgs()
	require.NoError(t, err)
	assert.Contains(t, args, "lease_epoch=4242")
}
