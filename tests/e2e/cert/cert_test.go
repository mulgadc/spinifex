//go:build e2e

package cert

import (
	"crypto/x509"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCertIssuance is the Go port of run-cert-e2e.sh.
//
// Validates that server certificates contain the correct SANs and that TLS
// connections succeed without InsecureSkipVerify when the Spinifex CA is
// trusted. Assumes the cluster is bootstrapped (services up, certs generated,
// CA installed in system trust store via bootstrap.sh step 8).
func TestCertIssuance(t *testing.T) {
	env := harness.LoadEnv(t)
	artifacts := harness.ArtifactDir(t, env)

	caPath, err := harness.ResolveCACert(env)
	require.NoError(t, err, "Spinifex CA cert must exist")
	t.Logf("CA cert: %s", caPath)
	t.Logf("mode=%s nodeIPs=%v serviceIPs=%v", env.Mode, env.NodeIPs, env.ServiceIPs)

	caCert, err := harness.ParseCertFile(caPath)
	require.NoError(t, err)

	t.Run("SANContainsNodeIPs", func(t *testing.T) {
		for _, ip := range env.NodeIPs {
			ip := ip
			t.Run(ip, func(t *testing.T) {
				t.Parallel()
				cert := loadCert(t, env, ip, artifacts)
				assert.True(t, harness.CertHasIPSAN(cert, ip),
					"cert for %s missing IP SAN %s (got IPs=%v DNS=%v)",
					ip, ip, cert.IPAddresses, cert.DNSNames)
			})
		}
	})

	t.Run("HostnameInDNSSANs", func(t *testing.T) {
		hostname, err := os.Hostname()
		require.NoError(t, err)
		if hostname == "" || hostname == "localhost" {
			t.Skipf("hostname %q not testable", hostname)
		}
		if env.Mode == harness.ModeMultinode || env.Mode == harness.ModeBaremetal {
			t.Skip("hostname SAN check requires local cert path")
		}
		referenceIP := env.NodeIPs[0]
		if env.Mode == harness.ModePseudo {
			referenceIP = "10.11.12.1"
		}
		cert := loadCert(t, env, referenceIP, artifacts)
		assert.True(t, harness.CertHasDNSSAN(cert, hostname),
			"cert missing DNS SAN for hostname %q (DNS=%v)", hostname, cert.DNSNames)
	})

	t.Run("TLSHandshakeWithCAFile", func(t *testing.T) {
		pool, err := harness.LoadCAPool(caPath)
		require.NoError(t, err)
		for _, ip := range env.ServiceIPs {
			ip := ip
			t.Run(ip, func(t *testing.T) {
				t.Parallel()
				url := fmt.Sprintf("https://%s:%d/", ip, env.AWSGWPort)
				code, _, err := harness.HTTPSGet(url, pool, env.DefaultTimeout)
				require.NoError(t, err, "TLS handshake to %s", url)
				assert.NotZero(t, code, "expected HTTP response from %s", url)
			})
		}
	})

	t.Run("SystemTrustStore", func(t *testing.T) {
		t.Run("CAInstalled", func(t *testing.T) {
			_, err := os.Stat(harness.SystemCAPath)
			require.NoError(t, err, "CA cert must be installed at %s", harness.SystemCAPath)
		})

		t.Run("FingerprintMatchesConfig", func(t *testing.T) {
			sysCert, err := harness.ParseCertFile(harness.SystemCAPath)
			require.NoError(t, err)
			assert.True(t, harness.FingerprintMatches(sysCert, caCert),
				"system CA fingerprint differs from %s — system trust store stale", caPath)
		})

		t.Run("CurlWithoutCAFlag", func(t *testing.T) {
			testIP := env.ServiceIPs[0]
			url := fmt.Sprintf("https://%s:%d/", testIP, env.AWSGWPort)
			code, _, err := harness.HTTPSGet(url, nil, env.DefaultTimeout)
			require.NoError(t, err, "system trust store: GET %s failed — CA not in system bundle?", url)
			assert.NotZero(t, code)
		})
	})

	t.Run("OpenSSLVerify", func(t *testing.T) {
		for _, ip := range env.ServiceIPs {
			ip := ip
			t.Run(ip, func(t *testing.T) {
				t.Parallel()
				require.NoError(t, harness.OpenSSLVerify(t, caPath, ip, env.AWSGWPort))
			})
		}
	})

	t.Run("CADownloadEndpoint", func(t *testing.T) {
		uiIP := env.NodeIPs[0]
		if env.Mode == harness.ModeSingle {
			uiIP = "127.0.0.1"
		}
		pool, err := harness.LoadCAPool(caPath)
		require.NoError(t, err)
		url := fmt.Sprintf("https://%s:%d/api/ca.pem", uiIP, env.UIPort)
		code, body, err := harness.HTTPSGet(url, pool, env.DefaultTimeout)
		if err != nil {
			t.Skipf("UI not reachable at %s: %v (pseudo-multinode disables UI)", url, err)
		}
		require.Equalf(t, 200, code, "GET %s returned %d", url, code)
		downloaded, err := harness.ParseCertPEM(body)
		require.NoError(t, err, "CA download must be a PEM cert")
		assert.True(t, harness.FingerprintMatches(downloaded, caCert),
			"downloaded CA fingerprint differs from local CA at %s", caPath)
	})

	t.Run("CertMetadata", func(t *testing.T) {
		if env.Mode == harness.ModeMultinode || env.Mode == harness.ModeBaremetal {
			t.Skip("metadata check requires local cert path")
		}
		referenceIP := env.NodeIPs[0]
		if env.Mode == harness.ModePseudo {
			referenceIP = "10.11.12.1"
		}
		cert := loadCert(t, env, referenceIP, artifacts)

		assert.Contains(t, cert.Subject.CommonName, "Spinifex Server",
			"server cert CN should be 'Spinifex Server' (got %q)", cert.Subject.CommonName)
		assert.Contains(t, cert.Issuer.CommonName, "Spinifex Local CA",
			"server cert issuer should be 'Spinifex Local CA' (got %q)", cert.Issuer.CommonName)
		assert.False(t, cert.NotAfter.IsZero(), "cert must have an expiry")
		t.Logf("cert expiry: %s", cert.NotAfter)
	})

	t.Run("CrossNodeIssuerSharedCA", func(t *testing.T) {
		if env.Mode == harness.ModeSingle || len(env.ServiceIPs) < 2 {
			t.Skip("requires multi-node deployment")
		}
		for _, ip := range env.ServiceIPs {
			ip := ip
			t.Run(ip, func(t *testing.T) {
				t.Parallel()
				served, err := harness.FetchServedCert(ip, env.AWSGWPort, env.DefaultTimeout)
				require.NoError(t, err)
				assert.Contains(t, served.Issuer.CommonName, "Spinifex Local CA",
					"node %s cert has unexpected issuer %q", ip, served.Issuer.CommonName)
			})
		}
	})

	harness.OnFailure(t, func() {
		harness.DumpCmd(t, artifacts, "ip-addr.txt", "ip", "-4", "addr", "show")
		harness.DumpCmd(t, artifacts, "ca-text.txt", "openssl", "x509", "-in", caPath, "-text", "-noout")
		if _, err := os.Stat(harness.SystemCAPath); err == nil {
			harness.DumpCmd(t, artifacts, "system-ca-text.txt",
				"openssl", "x509", "-in", harness.SystemCAPath, "-text", "-noout")
		}
	})
}

// loadCert resolves the cert for ip: local file when available, served cert
// otherwise. Dumps cert text to artifacts on failure.
func loadCert(t *testing.T, env *harness.Env, ip, artifacts string) *x509.Certificate {
	t.Helper()
	if path := harness.ServerCertPath(env, ip); path != "" {
		cert, err := harness.ParseCertFile(path)
		require.NoErrorf(t, err, "parse local cert %s", path)
		harness.OnFailure(t, func() {
			harness.DumpCmd(t, artifacts, sanitize("cert-"+ip+".txt"),
				"openssl", "x509", "-in", path, "-text", "-noout")
		})
		return cert
	}
	served, err := harness.FetchServedCert(ip, env.AWSGWPort, env.DefaultTimeout)
	require.NoErrorf(t, err, "fetch served cert %s:%d", ip, env.AWSGWPort)
	harness.OnFailure(t, func() {
		out, err := exec.Command("openssl", "s_client", "-connect",
			fmt.Sprintf("%s:%d", ip, env.AWSGWPort), "-servername", ip).CombinedOutput()
		if err != nil {
			out = append(out, []byte(fmt.Sprintf("\n(exit: %v)", err))...)
		}
		harness.DumpFile(t, artifacts, sanitize("served-"+ip+".txt"), out)
	})
	return served
}

func sanitize(s string) string { return strings.ReplaceAll(s, "/", "_") }
