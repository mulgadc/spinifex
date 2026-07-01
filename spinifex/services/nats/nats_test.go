package nats

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// templatePath returns the absolute path to the canonical nats.conf template
// used by admin init. Reading at test time (rather than embedding a copy)
// ensures the test always validates the real template.
func templatePath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "cmd", "spinifex", "cmd", "templates", "nats.conf")
}

// TestRenderedConfig_EnforcesAuth renders the production nats.conf template,
// starts an embedded NATS server, and verifies token authentication.
func TestRenderedConfig_EnforcesAuth(t *testing.T) {
	token := "nats_test-secret-token-1234"
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "nats.conf")

	// Generate test CA and server certs for TLS.
	caCertPath := filepath.Join(tmpDir, "ca.pem")
	caKeyPath := filepath.Join(tmpDir, "ca.key")
	require.NoError(t, admin.GenerateCACert(caCertPath, caKeyPath))
	serverCertPath := filepath.Join(tmpDir, "server.pem")
	serverKeyPath := filepath.Join(tmpDir, "server.key")
	require.NoError(t, admin.GenerateSignedCert(serverCertPath, serverKeyPath, caCertPath, caKeyPath, []string{"127.0.0.1"}, nil))

	// Read and render the production template.
	raw, err := os.ReadFile(templatePath(t))
	require.NoError(t, err)

	tmpl, err := template.New("nats.conf").Parse(string(raw))
	require.NoError(t, err)

	f, err := os.Create(confPath)
	require.NoError(t, err)

	err = tmpl.Execute(f, map[string]any{
		"BindIP":    "127.0.0.1",
		"Node":      "test-node",
		"NatsToken": token,
		"DataDir":   tmpDir,
		"LogDir":    tmpDir,
		"ConfigDir": tmpDir,
	})
	f.Close()
	require.NoError(t, err)

	// Start NATS server from the rendered config.
	opts, err := server.ProcessConfigFile(confPath)
	require.NoError(t, err)

	// Override for test isolation: random port, no monitoring.
	opts.Port = -1
	opts.LogFile = ""
	opts.HTTPHost = ""
	opts.HTTPPort = 0

	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	// Build TLS option using the test CA cert for client connections.
	tlsOpt := nats.RootCAs(caCertPath)

	t.Run("no token rejected", func(t *testing.T) {
		_, err := nats.Connect(ns.ClientURL(), tlsOpt, nats.MaxReconnects(0))
		assert.Error(t, err, "unauthenticated connection should be rejected")
	})

	t.Run("wrong token rejected", func(t *testing.T) {
		_, err := nats.Connect(ns.ClientURL(), tlsOpt, nats.Token("wrong-token"), nats.MaxReconnects(0))
		assert.Error(t, err, "wrong token should be rejected")
	})

	t.Run("correct token accepted", func(t *testing.T) {
		nc, err := nats.Connect(ns.ClientURL(), tlsOpt, nats.Token(token), nats.MaxReconnects(0))
		require.NoError(t, err, "correct token should be accepted")
		defer nc.Close()
		assert.True(t, nc.IsConnected())
	})

	t.Run("plaintext rejected", func(t *testing.T) {
		_, err := nats.Connect(ns.ClientURL(), nats.Token(token), nats.MaxReconnects(0))
		assert.Error(t, err, "plaintext connection should be rejected when TLS is enabled")
	})
}

// TestRenderedConfig_HasMigrationVersionMarker ensures the nats.conf template
// stamps the migration version on the first line (NATSConfVersionReader only
// checks line 0; missing marker causes fresh installs to run the 0→1 migration).
func TestRenderedConfig_HasMigrationVersionMarker(t *testing.T) {
	raw, err := os.ReadFile(templatePath(t))
	require.NoError(t, err)

	firstLine := strings.SplitN(string(raw), "\n", 2)[0]
	assert.Equal(t, "# spinifex-config-version: 3", firstLine,
		"nats.conf template must start with the current migration version marker")
}

func TestNew_ValidConfig(t *testing.T) {
	cfg := &Config{Port: 4222, Host: "127.0.0.1"}
	svc, err := New(cfg)
	require.NoError(t, err)
	require.NotNil(t, svc)
	assert.Same(t, cfg, svc.Config)
}

func TestNew_InvalidConfigType(t *testing.T) {
	svc, err := New("not a config")
	assert.Nil(t, svc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config type")
}
