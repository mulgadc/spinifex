package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The CP VM launches on an instance profile, so cloud-init omits the static
// EKS_ACCESS_KEY/EKS_SECRET_KEY and eksgw.New signs with IMDS creds. loadConfig
// must accept absent static keys; gating on them aborts every IMDS-mode boot,
// leaving the apiserver webhook kubeconfig unwritten.
func TestLoadConfig_AcceptsAbsentStaticCreds(t *testing.T) {
	setEnv := func(t *testing.T, kv map[string]string) {
		t.Helper()
		for _, k := range []string{
			"EKS_GATEWAY_URL", "EKS_ACCESS_KEY", "EKS_SECRET_KEY",
			"EKS_ACCOUNT_ID", "EKS_CLUSTER_NAME",
		} {
			t.Setenv(k, "")
		}
		for k, v := range kv {
			t.Setenv(k, v)
		}
	}

	// IMDS mode: required vars set, static creds absent → accepted.
	setEnv(t, map[string]string{
		"EKS_GATEWAY_URL":  "https://gw:9999",
		"EKS_ACCOUNT_ID":   "000000000001",
		"EKS_CLUSTER_NAME": "toc",
	})
	cfg, err := loadConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.AccessKey)
	assert.Empty(t, cfg.SecretKey)

	// Genuinely-required vars are still enforced.
	setEnv(t, map[string]string{
		"EKS_ACCOUNT_ID":   "000000000001",
		"EKS_CLUSTER_NAME": "toc",
	})
	_, err = loadConfig()
	require.Error(t, err)
}
