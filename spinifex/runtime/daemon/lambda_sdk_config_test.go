//go:build lambdatestlink

//test:in-package exercises daemon-owned gateway configuration
package daemon

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLambdaSDKConfigUsesManagementGatewayAndClusterCA(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, ca, 0o600))
	d := &Daemon{mgmtBridgeIP: "10.15.8.1", config: &config.Config{
		NATS: config.NATSConfig{CACert: path}, AWSGW: config.AWSGWConfig{Host: "0.0.0.0:9999"},
	}}
	endpoint, gotCA, err := d.lambdaTestLinkSDKConfig()
	require.NoError(t, err)
	assert.Equal(t, "https://10.15.8.1:9999", endpoint)
	assert.Equal(t, ca, gotCA)

	withKey := append(append([]byte{}, ca...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("private")})...)
	withKey = append(withKey, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")})...)
	require.NoError(t, os.WriteFile(path, withKey, 0o600))
	_, gotCA, err = d.lambdaTestLinkSDKConfig()
	require.NoError(t, err)
	assert.Equal(t, ca, gotCA, "only public certificates reach the guest")

	require.NoError(t, os.WriteFile(path, []byte("bad CA"), 0o600))
	_, _, err = d.lambdaTestLinkSDKConfig()
	require.Error(t, err)
	_, err = d.lambdaTestLinkHTTP()
	require.Error(t, err, "configured invalid download trust must still refuse serving")
}
