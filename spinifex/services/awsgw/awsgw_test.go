package awsgw

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadThrottleConfig_Enabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "awsgw.toml")
	content := `
version = 2
region = "us-east-1"

[ratelimit]
enabled = true
rate = 20
burst = 100

[ratelimit.action.RunInstances]
rate = 2
burst = 40
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	parsed, err := loadAWSGWConfig(path)
	require.NoError(t, err)
	cfg := parsed.Ratelimit
	assert.True(t, cfg.Enabled)
	assert.Equal(t, 20, cfg.Rate)
	assert.Equal(t, 100, cfg.Burst)
	assert.Equal(t, 2, cfg.Action["RunInstances"].Rate)
	assert.Equal(t, 40, cfg.Action["RunInstances"].Burst)
}

func TestLoadThrottleConfig_Disabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "awsgw.toml")
	content := `
version = 2
[ratelimit]
enabled = false
rate = 20
burst = 100
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	parsed, err := loadAWSGWConfig(path)
	require.NoError(t, err)
	assert.False(t, parsed.Ratelimit.Enabled)
}

func TestLoadThrottleConfig_NoSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "awsgw.toml")
	content := `
version = "1.0"
region = "us-east-1"
debug = false
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	parsed, err := loadAWSGWConfig(path)
	require.NoError(t, err)
	// Missing section → zero-value config (disabled, rate=0, burst=0).
	assert.False(t, parsed.Ratelimit.Enabled)
	assert.Equal(t, 0, parsed.Ratelimit.Rate)
}

func TestLoadAWSGWConfig_MissingFile(t *testing.T) {
	_, err := loadAWSGWConfig("/nonexistent/awsgw.toml")
	assert.Error(t, err)
}

func TestLoadQuotaConfig_Enabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "awsgw.toml")
	content := `
version = "3"
region = "us-east-1"

[quota]
enabled     = true
vcpus       = 8
vpcs        = 8
subnets     = 16
eips        = 2
volumes_gib = 100
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	parsed, err := loadAWSGWConfig(path)
	require.NoError(t, err)
	cfg := parsed.Quota
	assert.True(t, cfg.Enabled)
	assert.Equal(t, 8, cfg.VCPUs)
	assert.Equal(t, 8, cfg.VPCs)
	assert.Equal(t, 16, cfg.Subnets)
	assert.Equal(t, 2, cfg.EIPs)
	assert.Equal(t, 100, cfg.VolumesGiB)
}

func TestLoadQuotaConfig_Disabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "awsgw.toml")
	content := `
version = "3"
[quota]
enabled = false
vcpus   = 8
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	parsed, err := loadAWSGWConfig(path)
	require.NoError(t, err)
	assert.False(t, parsed.Quota.Enabled)
}

func TestLoadQuotaConfig_NoSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "awsgw.toml")
	content := `
version = "3"
region = "us-east-1"
debug = false
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	parsed, err := loadAWSGWConfig(path)
	require.NoError(t, err)
	// Missing section → zero-value Limits, a disabled no-op.
	assert.False(t, parsed.Quota.Enabled)
	assert.Equal(t, 0, parsed.Quota.VCPUs)
}
