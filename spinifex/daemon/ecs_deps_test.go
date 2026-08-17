// Exercises buildECSServiceDeps, which is unexported wiring with no exported
// surface to drive it through.
//
//test:in-package
package daemon

import (
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A worker with no master key still serves the rest of ECS. Capacity
// provisioning is the only thing the IAM service gates, so disabling it must
// not take the gateway URL, the image resolver or the launcher with it.
func TestBuildECSServiceDeps_NoMasterKeyDisablesOnlyCapacity(t *testing.T) {
	d := &Daemon{
		config:     &config.Config{},
		configPath: filepath.Join(t.TempDir(), "spinifex.toml"),
	}

	deps := d.buildECSServiceDeps()

	assert.Nil(t, deps.IAM, "capacity provisioning is disabled without a master key")
	require.NotNil(t, deps.RunInstances, "the launcher is independent of IAM")
}
