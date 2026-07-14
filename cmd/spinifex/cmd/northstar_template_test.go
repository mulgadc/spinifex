package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/admin"
	toml "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renderTemplate(t *testing.T, tmpl string, settings admin.ConfigSettings) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.toml")
	require.NoError(t, admin.GenerateConfigFile(path, tmpl, settings))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, toml.Unmarshal(data, &parsed), "rendered template must be valid TOML")
	return string(data)
}

func northstarSettings() admin.ConfigSettings {
	return admin.ConfigSettings{
		Region:                  "ap-southeast-2",
		BindIP:                  "0.0.0.0",
		AdvertiseIP:             "192.168.1.31",
		AccessKey:               "AKIASYSTEM",
		SecretKey:               "SYSTEMSECRET",
		NatsToken:               "NATSTOKEN",
		PoolDNSServers:          []string{"1.1.1.1", "8.8.8.8"},
		NorthstarAccessKey:      "AKIANORTHSTAR",
		NorthstarSecretKey:      "NORTHSTARSECRET",
		NorthstarBucket:         admin.NorthstarBucketName,
		NorthstarDefaultDomain:  admin.NorthstarDefaultDomain,
		NorthstarInternalDomain: admin.NorthstarInternalDomain,
	}
}

func TestNorthstarTomlTemplate(t *testing.T) {
	content := renderTemplate(t, northstarTomlTemplate, northstarSettings())

	// Northstar binds :5300 on the wildcard and the privileged :53 on the node
	// WAN IP (avoids the systemd-resolved stub on 127.0.0.53:53).
	assert.Contains(t, content, `listen     = "0.0.0.0:5300,192.168.1.31:53"`)
	assert.Contains(t, content, `default_domain = "spx3.net"`)
	assert.Contains(t, content, `bucket     = "northstar"`)
	assert.Contains(t, content, `access_key = "AKIANORTHSTAR"`)
	assert.Contains(t, content, `secret_key = "NORTHSTARSECRET"`)
	// 0.0.0.0 bind must resolve to a loopback S3 endpoint, not a dial to 0.0.0.0.
	assert.Contains(t, content, `endpoint   = "https://127.0.0.1:8443"`)
	assert.Contains(t, content, `nameservers = ["1.1.1.1:53", "8.8.8.8:53"]`)
	assert.Contains(t, content, `internal_domain = "compute.internal"`)
	assert.NotContains(t, content, "nats_url")
	assert.NotContains(t, content, "NATSTOKEN")
}

// When provisioned, predastore.toml gains the northstar bucket plus a scoped
// read-only [[auth]] entry — the isolation guarantee.
func TestPredastoreTemplateWithNorthstar(t *testing.T) {
	content := renderTemplate(t, predastoreTomlTemplate, northstarSettings())

	assert.Contains(t, content, `name = "northstar"`)
	assert.Contains(t, content, `access_key_id = "AKIANORTHSTAR"`)
	assert.Contains(t, content, `{ bucket = "northstar", actions = ["s3:ListBucket", "s3:GetObject"] }`)
}

// Without northstar creds (e.g. cluster-join paths), predastore.toml renders
// exactly as before — no empty bucket/auth stanzas.
func TestPredastoreTemplateWithoutNorthstar(t *testing.T) {
	s := northstarSettings()
	s.NorthstarAccessKey = ""
	s.NorthstarSecretKey = ""
	content := renderTemplate(t, predastoreTomlTemplate, s)

	assert.NotContains(t, content, `name = "northstar"`)
	assert.NotContains(t, content, "AKIANORTHSTAR")
}
