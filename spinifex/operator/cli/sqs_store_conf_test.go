//test:in-package — the embedded config templates it renders are unexported,
// and exporting them would widen the package's API for a test's convenience.

package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/sqsstore"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestCerts writes server.pem, server.key and ca.pem under dir so the
// rendered configs, which name them in their tls blocks, load.
func writeTestCerts(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.pem"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.pem"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.key"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
}

// renderBoth renders the control-plane and SQS store configs from one set of
// node settings, exactly as admin init does, and parses each with nats-server.
func renderBoth(t *testing.T, routes []string) (control, store *natsserver.Options) {
	t.Helper()
	root := t.TempDir()
	writeTestCerts(t, root)
	dirs, err := createConfigSubdirs(root)
	require.NoError(t, err)

	settings := admin.ConfigSettings{
		Node: "node1", BindIP: "10.0.0.1", ClusterBindIP: "10.0.0.1", ClusterName: "spinifex",
		NatsToken: "nats_control", SQSStoreToken: "nats_sqsstore",
		ConfigDir: root, DataDir: filepath.Join(root, "data"), LogDir: filepath.Join(root, "log"),
	}
	if routes != nil {
		settings.ClusterRoutes = routes
		settings.SQSStoreRoutes = admin.SQSStoreRoutes(routes)
	}
	controlPath := filepath.Join(dirs.NATS, "nats.conf")
	storePath := filepath.Join(dirs.SQSStore, "sqs-store.conf")
	require.NoError(t, admin.GenerateConfigFile(controlPath, natsConfTemplate, settings))
	require.NoError(t, admin.GenerateConfigFile(storePath, sqsStoreConfTemplate, settings))

	control, err = natsserver.ProcessConfigFile(controlPath)
	require.NoError(t, err)
	store, err = natsserver.ProcessConfigFile(storePath)
	require.NoError(t, err)
	return control, store
}

func TestSQSStoreConfSharesNothingWithControlPlane(t *testing.T) {
	control, store := renderBoth(t, []string{"10.0.0.1:4248", "10.0.0.2:4248", "10.0.0.3:4248"})

	assert.NotEqual(t, control.Port, store.Port, "client port")
	assert.NotEqual(t, control.HTTPPort, store.HTTPPort, "monitoring port")
	assert.NotEqual(t, control.Cluster.Port, store.Cluster.Port, "cluster port")
	assert.NotEqual(t, control.Cluster.Name, store.Cluster.Name, "cluster name")
	assert.NotEqual(t, control.StoreDir, store.StoreDir, "store directory")
	assert.NotEqual(t, control.ServerName, store.ServerName, "server name")
	assert.NotEqual(t, control.Authorization, store.Authorization, "client token")
	assert.NotEqual(t, control.Cluster.Password, store.Cluster.Password, "route credential")
	assert.NotEqual(t, control.LogFile, store.LogFile, "log file")

	assert.Empty(t, store.LeafNode.Host+store.Gateway.Host, "no leaf or gateway")
	require.Len(t, store.Routes, 3)
	for _, r := range store.Routes {
		assert.Equal(t, "4249", r.Port(), "store routes target the store cluster port")
	}
	for _, r := range control.Routes {
		assert.Equal(t, "4248", r.Port())
	}
}

func TestSQSStoreConfDurabilityAndPayload(t *testing.T) {
	control, store := renderBoth(t, nil)

	assert.True(t, store.SyncAlways, "store must fsync before acknowledging")
	assert.Equal(t, int32(2*1024*1024), store.MaxPayload)
	assert.NoError(t, sqsstore.ValidateOptions(store))

	assert.Equal(t, int32(1024*1024), control.MaxPayload, "control-plane max_payload is unchanged")
	assert.False(t, control.SyncAlways, "control-plane sync_interval is unchanged")
	assert.Error(t, sqsstore.ValidateOptions(control), "the control-plane config is not a valid store config")
}

func TestSQSStoreConfOmitsClusterOnSingleNode(t *testing.T) {
	_, store := renderBoth(t, nil)
	assert.Empty(t, store.Routes)
	assert.Zero(t, store.Cluster.Port)
}

func TestSQSStoreTokenNeverInControlPlaneConfigs(t *testing.T) {
	root := t.TempDir()
	dirs, err := createConfigSubdirs(root)
	require.NoError(t, err)
	settings := admin.ConfigSettings{
		Node: "node1", NatsToken: "nats_control", SQSStoreToken: "nats_sqsstore",
		ConfigDir: root, DataDir: root, LogDir: root, Region: "r", Az: "a", Port: "9999",
	}
	require.NoError(t, generateAndWriteConfigs(dirs, filepath.Join(root, "spinifex.toml"), settings, true))

	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		require.NoError(t, rerr)
		inStore := strings.HasPrefix(p, dirs.SQSStore+string(filepath.Separator))
		assert.Equal(t, inStore, strings.Contains(string(b), "nats_sqsstore"), "%s", p)
		return nil
	})
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dirs.SQSStore, "sqs-store.conf"))
	assert.NoError(t, err)
}

func TestSQSStoreConfSkippedWithoutToken(t *testing.T) {
	root := t.TempDir()
	dirs, err := createConfigSubdirs(root)
	require.NoError(t, err)
	settings := admin.ConfigSettings{Node: "node1", NatsToken: "t", ConfigDir: root, DataDir: root, LogDir: root, Region: "r", Az: "a", Port: "9999"}
	require.NoError(t, generateAndWriteConfigs(dirs, filepath.Join(root, "spinifex.toml"), settings, true))
	_, err = os.Stat(filepath.Join(dirs.SQSStore, "sqs-store.conf"))
	assert.True(t, os.IsNotExist(err))
}

// TestControlPlaneNATSTemplateUnchanged pins the control-plane template so a
// change to it is deliberate and reviewed apart from the SQS store.
func TestControlPlaneNATSTemplateUnchanged(t *testing.T) {
	sum := sha256.Sum256([]byte(natsConfTemplate))
	assert.Equal(t, "afb95c63d6cbe892271656306db8540975a3d839b05ba2cf74895f0d6b6d4729", hex.EncodeToString(sum[:]))
}
