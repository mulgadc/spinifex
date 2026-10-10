package awsgw

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wireServiceRegistry is the exact call launchService makes to wire
// gw.Services before serving a request. Production's ECR and ACM
// registrations must wire an overlap-free registry serving ecr and acm; a
// duplicate or a registration claiming a legacy name must fail it, so the
// role refuses to start.
func TestWireServiceRegistry(t *testing.T) {
	t.Run("ECR registration wires and passes the overlap guard", func(t *testing.T) {
		gw := &gateway.GatewayConfig{}
		require.NoError(t, wireServiceRegistry(gw, awsapi.NewRegistration(awsapi.Deps{})))
		require.NotNil(t, gw.Services)
		_, ok := gw.Services.Lookup("ecr")
		assert.True(t, ok)
	})

	t.Run("a duplicate ECR registration fails startup", func(t *testing.T) {
		gw := &gateway.GatewayConfig{}
		reg := awsapi.NewRegistration(awsapi.Deps{})
		require.Error(t, wireServiceRegistry(gw, reg, reg))
	})

	t.Run("ACM registration wires and passes the overlap guard", func(t *testing.T) {
		gw := &gateway.GatewayConfig{}
		require.NoError(t, wireServiceRegistry(gw, acmawsapi.NewRegistration(acmawsapi.Deps{})))
		require.NotNil(t, gw.Services)
		_, ok := gw.Services.Lookup("acm")
		assert.True(t, ok)
	})

	t.Run("a duplicate ACM registration fails startup", func(t *testing.T) {
		gw := &gateway.GatewayConfig{}
		reg := acmawsapi.NewRegistration(acmawsapi.Deps{})
		require.Error(t, wireServiceRegistry(gw, reg, reg))
	})

	t.Run("ECR and ACM registrations wire together, as production does", func(t *testing.T) {
		gw := &gateway.GatewayConfig{}
		require.NoError(t, wireServiceRegistry(gw,
			awsapi.NewRegistration(awsapi.Deps{}), acmawsapi.NewRegistration(acmawsapi.Deps{})))
		require.NotNil(t, gw.Services)
		_, ecrOK := gw.Services.Lookup("ecr")
		_, acmOK := gw.Services.Lookup("acm")
		assert.True(t, ecrOK)
		assert.True(t, acmOK)
	})

	t.Run("a registration claiming a legacy name fails startup", func(t *testing.T) {
		gw := &gateway.GatewayConfig{}
		err := wireServiceRegistry(gw, dispatch.Registration{
			Service:   "ec2",
			Dispatch:  func(http.ResponseWriter, dispatch.Invocation) error { return nil },
			Errors:    dispatch.ErrorEnvelopeJSON,
			Inventory: dispatch.Inventory{Registered: []string{"Whatever"}},
		})
		require.Error(t, err)
	})
}

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

// An absent [signup] section must cap self-service creation rather than leave
// it open, while an explicit 0 stays the documented way to lift the cap.
func TestResolveSignupMaxAccounts(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{
			name:    "no section defaults",
			content: "version = \"3\"\n",
			want:    defaultSignupMaxAccounts,
		},
		{
			name:    "section without key defaults",
			content: "version = \"3\"\n[signup]\n",
			want:    defaultSignupMaxAccounts,
		},
		{
			name:    "explicit zero is uncapped",
			content: "version = \"3\"\n[signup]\nmax_accounts = 0\n",
			want:    0,
		},
		{
			name:    "explicit value wins",
			content: "version = \"3\"\n[signup]\nmax_accounts = 4\n",
			want:    4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "awsgw.toml")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o644))

			parsed, err := loadAWSGWConfig(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, resolveSignupMaxAccounts(parsed.Signup))
		})
	}
}
