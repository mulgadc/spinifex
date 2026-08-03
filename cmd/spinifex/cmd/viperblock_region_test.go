package cmd

import (
	"testing"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The viperblock start command applies each of these over spinifex.toml whenever
// viper reports a non-empty value, so a non-empty flag default makes the override
// unconditional and discards the configured value on every start.
func TestViperblockConfigOverrideFlagsDefaultEmpty(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"s3-host", "s3-bucket", "s3-region"} {
		flag := viperblockCmd.PersistentFlags().Lookup(name)
		require.NotNil(t, flag, "flag %s must exist", name)
		assert.Empty(t, flag.DefValue, "flag %s must default empty so an unset flag cannot clobber spinifex.toml", name)
	}
}

// predastore verifies each request's SigV4 credential scope against its own
// configured region, so a bucket pinned to a literal region breaks every client
// on a cluster deployed anywhere else.
func TestPredastoreTemplateBucketsUseConfiguredRegion(t *testing.T) {
	t.Parallel()

	settings := northstarSettings()
	settings.Region = "bare-metal"
	content := renderTemplate(t, predastoreTomlTemplate, settings)

	var parsed struct {
		Region  string `toml:"region"`
		Buckets []struct {
			Name   string `toml:"name"`
			Region string `toml:"region"`
		} `toml:"buckets"`
	}
	require.NoError(t, toml.Unmarshal([]byte(content), &parsed))

	assert.Equal(t, "bare-metal", parsed.Region)
	require.NotEmpty(t, parsed.Buckets)
	for _, bucket := range parsed.Buckets {
		assert.Equal(t, "bare-metal", bucket.Region, "bucket %s", bucket.Name)
	}
}
