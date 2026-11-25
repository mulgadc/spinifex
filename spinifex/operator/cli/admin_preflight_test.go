package cli_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/preflight"
	"github.com/mulgadc/spinifex/spinifex/operator/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// make deploy gates on the asset-only set before swapping spx; a plain run
// must keep the full set, running-binary check included.
func TestAdminPreflightSkipRunningBinaries(t *testing.T) {
	flag := cli.AdminPreflightCmd.Flags().Lookup("skip-running-binaries")
	require.NotNil(t, flag)
	assert.Equal(t, "false", flag.DefValue)

	var called []string
	stale := []preflight.Result{{Path: "spinifex-daemon.service", Kind: "service", Status: preflight.Stale}}
	t.Cleanup(cli.StubPreflightChecks(
		func() []preflight.Result { called = append(called, "full"); return stale },
		func() []preflight.Result { called = append(called, "assets"); return nil },
	))
	t.Cleanup(func() { _ = flag.Value.Set("false") })

	require.Error(t, cli.RunAdminPreflight(cli.AdminPreflightCmd, nil))
	assert.Equal(t, []string{"full"}, called)

	called = nil
	require.NoError(t, flag.Value.Set("true"))
	require.NoError(t, cli.RunAdminPreflight(cli.AdminPreflightCmd, nil))
	assert.Equal(t, []string{"assets"}, called)
}
