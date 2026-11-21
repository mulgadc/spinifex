package cli

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An init writes map_format_default = 2; a join, which leaves it unset,
// writes nothing, so the joining node resolves format 1.
func TestSpinifexTomlTemplate_MapFormatDefaultOnlyOnInit(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  int
		want *int
	}{{"init", 2, new(2)}, {"join", 0, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			path := renderSpinifexToml(t, admin.ConfigSettings{MapFormatDefault: tc.set})
			viper.Reset()
			t.Cleanup(viper.Reset)
			cfg, err := config.LoadConfig(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Nodes["node1"].Viperblock.MapFormatDefault)
		})
	}
}
