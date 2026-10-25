package cli

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readServerCert(t *testing.T, configDir string) *x509.Certificate {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(configDir, "server.pem"))
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func certIPs(cert *x509.Certificate) []string {
	ips := make([]string, 0, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		ips = append(ips, ip.String())
	}
	return ips
}

// A renew must keep every SAN init minted, taking region and services domain
// from the config, and add the operator's extras on top.
func TestRenewServerCert_KeepsInitSANs(t *testing.T) {
	configDir := t.TempDir()
	const (
		region   = "ap-southeast-2"
		domain   = "corp.internal"
		bindIP   = "203.0.113.7"
		extraIP  = "198.51.100.9"
		extraDNS = "spx.example.com"
	)
	require.NoError(t, admin.GenerateCACert(filepath.Join(configDir, "ca.pem"), filepath.Join(configDir, "ca.key")))
	require.NoError(t, admin.GenerateServerCertOnly(configDir, bindIP, region, domain))
	initCert := readServerCert(t, configDir)

	cfg := &config.ClusterConfig{
		Node:  "node1",
		AWS:   config.AWSConfig{Region: region, ServicesDomain: domain},
		Nodes: map[string]config.Config{"node1": {Host: bindIP}},
	}
	require.NoError(t, renewServerCert(configDir, cfg, []string{extraIP}, []string{extraDNS}))
	renewed := readServerCert(t, configDir)

	require.NotEqual(t, initCert.SerialNumber, renewed.SerialNumber, "cert was not reissued")
	assert.Subset(t, renewed.DNSNames, initCert.DNSNames)
	assert.Subset(t, certIPs(renewed), certIPs(initCert))

	serviceNames := admin.AWSGWServiceDNSNames(region, domain)
	require.NotEmpty(t, serviceNames)
	assert.Subset(t, renewed.DNSNames, append(serviceNames, extraDNS))
	assert.Subset(t, certIPs(renewed), []string{config.DefaultMgmtBridgeIP, bindIP, extraIP})
}
