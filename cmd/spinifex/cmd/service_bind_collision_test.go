package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
)

// bindAllServiceCollisionEnv mirrors service.go's init() registration for
// the predastore/nats/awsgw keys that used to collide, against a freshly
// Reset viper instance. pflags survive Reset: they live on predastoreCmd,
// natsCmd and awsgwCmd, package-level command singletons.
func bindAllServiceCollisionEnv(t *testing.T) {
	t.Helper()
	viper.SetEnvPrefix("SPINIFEX")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()
	bindPredastoreCollisionEnv()
	bindNatsCollisionEnv()
	bindAwsgwCollisionEnv()
}

// TestSubcommandDebugKeysResolveIndependently proves predastore, nats and
// awsgw each resolve their own SPINIFEX_<SERVICE>_DEBUG value instead of
// sharing one last-registration-wins "debug" viper key.
func TestSubcommandDebugKeysResolveIndependently(t *testing.T) {
	t.Cleanup(func() { viper.Reset() })
	viper.Reset()
	bindAllServiceCollisionEnv(t)

	t.Setenv("SPINIFEX_PREDASTORE_DEBUG", "true")
	t.Setenv("SPINIFEX_NATS_DEBUG", "false")
	t.Setenv("SPINIFEX_AWSGW_DEBUG", "true")

	assert.True(t, viper.GetBool("predastore-debug"))
	assert.False(t, viper.GetBool("nats-debug"))
	assert.True(t, viper.GetBool("awsgw-debug"))
}

// TestSubcommandPortKeysResolveIndependently proves predastore and nats
// each resolve their own SPINIFEX_<SERVICE>_PORT value instead of sharing
// one last-registration-wins "port" viper key.
func TestSubcommandPortKeysResolveIndependently(t *testing.T) {
	t.Cleanup(func() { viper.Reset() })
	viper.Reset()
	bindAllServiceCollisionEnv(t)

	t.Setenv("SPINIFEX_PREDASTORE_PORT", "9001")
	t.Setenv("SPINIFEX_NATS_PORT", "9002")

	assert.Equal(t, 9001, viper.GetInt("predastore-port"))
	assert.Equal(t, 9002, viper.GetInt("nats-port"))
}

// TestSubcommandHostKeysResolveIndependently proves nats and awsgw each
// resolve their own SPINIFEX_<SERVICE>_HOST value instead of sharing one
// last-registration-wins "host" viper key.
func TestSubcommandHostKeysResolveIndependently(t *testing.T) {
	t.Cleanup(func() { viper.Reset() })
	viper.Reset()
	bindAllServiceCollisionEnv(t)

	t.Setenv("SPINIFEX_NATS_HOST", "10.0.0.1")
	t.Setenv("SPINIFEX_AWSGW_HOST", "10.0.0.2")

	assert.Equal(t, "10.0.0.1", viper.GetString("nats-service-host"))
	assert.Equal(t, "10.0.0.2", viper.GetString("awsgw-host"))
}

// TestSubcommandTLSKeysResolveIndependently proves predastore and awsgw
// each resolve their own SPINIFEX_<SERVICE>_TLS_CERT/KEY values instead of
// sharing one last-registration-wins "tls-cert"/"tls-key" viper key.
func TestSubcommandTLSKeysResolveIndependently(t *testing.T) {
	t.Cleanup(func() { viper.Reset() })
	viper.Reset()
	bindAllServiceCollisionEnv(t)

	t.Setenv("SPINIFEX_PREDASTORE_TLS_CERT", "/etc/predastore/cert.pem")
	t.Setenv("SPINIFEX_PREDASTORE_TLS_KEY", "/etc/predastore/key.pem")
	t.Setenv("SPINIFEX_AWSGW_TLS_CERT", "/etc/awsgw/cert.pem")
	t.Setenv("SPINIFEX_AWSGW_TLS_KEY", "/etc/awsgw/key.pem")

	assert.Equal(t, "/etc/predastore/cert.pem", viper.GetString("predastore-tls-cert"))
	assert.Equal(t, "/etc/predastore/key.pem", viper.GetString("predastore-tls-key"))
	assert.Equal(t, "/etc/awsgw/cert.pem", viper.GetString("awsgw-tls-cert"))
	assert.Equal(t, "/etc/awsgw/key.pem", viper.GetString("awsgw-tls-key"))
}

// TestGenericSpinifexEnvDoesNotShadowNamespacedKeys proves a generic
// SPINIFEX_DEBUG/SPINIFEX_HOST/SPINIFEX_PORT env var — the exact shape of
// the P0 shadow mechanism fixed for predastore in #659 — cannot silently
// override any per-subcommand value, because AutomaticEnv's derived name for
// each namespaced key no longer matches a generic bare name.
func TestGenericSpinifexEnvDoesNotShadowNamespacedKeys(t *testing.T) {
	t.Cleanup(func() { viper.Reset() })
	viper.Reset()
	bindAllServiceCollisionEnv(t)

	t.Setenv("SPINIFEX_DEBUG", "true")
	t.Setenv("SPINIFEX_HOST", "255.255.255.255")
	t.Setenv("SPINIFEX_PORT", "1")

	t.Setenv("SPINIFEX_PREDASTORE_DEBUG", "false")
	t.Setenv("SPINIFEX_NATS_DEBUG", "false")
	t.Setenv("SPINIFEX_AWSGW_DEBUG", "false")
	t.Setenv("SPINIFEX_NATS_HOST", "10.0.0.9")
	t.Setenv("SPINIFEX_AWSGW_HOST", "10.0.0.10")
	t.Setenv("SPINIFEX_PREDASTORE_PORT", "8443")
	t.Setenv("SPINIFEX_NATS_PORT", "4222")

	assert.False(t, viper.GetBool("predastore-debug"), "generic SPINIFEX_DEBUG must not shadow predastore-debug")
	assert.False(t, viper.GetBool("nats-debug"), "generic SPINIFEX_DEBUG must not shadow nats-debug")
	assert.False(t, viper.GetBool("awsgw-debug"), "generic SPINIFEX_DEBUG must not shadow awsgw-debug")
	assert.Equal(t, "10.0.0.9", viper.GetString("nats-service-host"), "generic SPINIFEX_HOST must not shadow nats-service-host")
	assert.Equal(t, "10.0.0.10", viper.GetString("awsgw-host"), "generic SPINIFEX_HOST must not shadow awsgw-host")
	assert.Equal(t, 8443, viper.GetInt("predastore-port"), "generic SPINIFEX_PORT must not shadow predastore-port")
	assert.Equal(t, 4222, viper.GetInt("nats-port"), "generic SPINIFEX_PORT must not shadow nats-port")
}
