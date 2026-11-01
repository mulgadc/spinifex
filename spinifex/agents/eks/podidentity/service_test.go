package podidentity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/konnectivitycert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeCA writes a self-signed CA cert and key as <name>.crt and <name>.key.
func writeCA(t *testing.T, dir, name string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	kd, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600))
	ca, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return ca
}

func certTestConfig(t *testing.T) (CertConfig, string) {
	t.Helper()
	tls := t.TempDir()
	writeCA(t, tls, "server-ca")
	writeCA(t, tls, "client-ca")
	return CertConfig{
		Dir:          filepath.Join(t.TempDir(), "state"),
		ServerCACert: filepath.Join(tls, "server-ca.crt"), ServerCAKey: filepath.Join(tls, "server-ca.key"),
		ClientCACert: filepath.Join(tls, "client-ca.crt"), ClientCAKey: filepath.Join(tls, "client-ca.key"),
	}, tls
}

func parseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	blk, _ := pem.Decode(b)
	require.NotNil(t, blk)
	c, err := x509.ParseCertificate(blk.Bytes)
	require.NoError(t, err)
	return c
}

func TestEnsureCerts_ServingCertIsForLoopbackAndSignedByTheServerCA(t *testing.T) {
	cfg, _ := certTestConfig(t)
	out, err := EnsureCerts(cfg)
	require.NoError(t, err)

	serverCA := parseCert(t, cfg.ServerCACert)
	serving := parseCert(t, out.ServingCert)
	require.NoError(t, serving.CheckSignatureFrom(serverCA))
	require.Len(t, serving.IPAddresses, 1)
	assert.True(t, serving.IPAddresses[0].IsLoopback())
	assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, serving.ExtKeyUsage)

	roots := x509.NewCertPool()
	roots.AddCert(serverCA)
	_, err = serving.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1"})
	assert.NoError(t, err, "the caBundle (server CA) must validate the serving cert for 127.0.0.1")
}

func TestEnsureCerts_ClientCertIsSignedByTheClientCAWithTheWebhookIdentity(t *testing.T) {
	cfg, _ := certTestConfig(t)
	out, err := EnsureCerts(cfg)
	require.NoError(t, err)

	client := parseCert(t, out.ClientCert)
	require.NoError(t, client.CheckSignatureFrom(parseCert(t, cfg.ClientCACert)))
	assert.Equal(t, ClientCN, client.Subject.CommonName)
	assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, client.ExtKeyUsage)
}

func TestEnsureCerts_KeysAreOwnerOnly(t *testing.T) {
	cfg, _ := certTestConfig(t)
	out, err := EnsureCerts(cfg)
	require.NoError(t, err)
	for _, k := range []string{out.ServingKey, out.ClientKey} {
		st, err := os.Stat(k)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), k)
	}
}

func TestEnsureCerts_ReusesValidCertsAndReissuesWhenTheCAChanges(t *testing.T) {
	cfg, tls := certTestConfig(t)
	first, err := EnsureCerts(cfg)
	require.NoError(t, err)
	before, err := os.ReadFile(first.ServingCert)
	require.NoError(t, err)

	_, err = EnsureCerts(cfg)
	require.NoError(t, err)
	same, err := os.ReadFile(first.ServingCert)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(before, same), "a valid cert must be reused so restarts do not churn it")

	writeCA(t, tls, "server-ca") // the cluster CA is replaced
	_, err = EnsureCerts(cfg)
	require.NoError(t, err)
	after, err := os.ReadFile(first.ServingCert)
	require.NoError(t, err)
	assert.False(t, bytes.Equal(before, after), "a cert signed by a retired CA must be reissued")
	require.NoError(t, parseCert(t, first.ServingCert).CheckSignatureFrom(parseCert(t, cfg.ServerCACert)))
}

func TestEnsureCerts_MissingCAFails(t *testing.T) {
	cfg, _ := certTestConfig(t)
	cfg.ServerCACert = filepath.Join(t.TempDir(), "absent.crt")
	_, err := EnsureCerts(cfg)
	assert.Error(t, err)
}

func TestRenderManifests_GrantsLeastPrivilegeAndEmbedsTheWebhook(t *testing.T) {
	out, err := RenderManifests(WebhookConfig{Name: "n", URL: "https://127.0.0.1:8444/mutate", CA: []byte("ca")})
	require.NoError(t, err)
	docs := strings.Split(string(out), "---\n")
	require.Len(t, docs, 3)

	var role struct {
		Rules []struct {
			Resources     []string `json:"resources"`
			ResourceNames []string `json:"resourceNames"`
			Verbs         []string `json:"verbs"`
		} `json:"rules"`
	}
	require.NoError(t, json.Unmarshal([]byte(docs[0]), &role))
	got := map[string][]string{}
	for _, r := range role.Rules {
		for _, res := range r.Resources {
			key := res
			if len(r.ResourceNames) > 0 {
				key += "/named"
			}
			got[key] = append(got[key], r.Verbs...)
		}
		assert.NotContains(t, r.Verbs, "*")
		assert.NotContains(t, r.Verbs, "delete")
	}
	assert.Equal(t, map[string][]string{
		"serviceaccounts":  {"get"},
		"namespaces":       {"list", "watch"},
		"configmaps":       {"create"},
		"configmaps/named": {"get", "update"},
	}, got)

	var binding struct {
		Subjects []struct {
			Name string `json:"name"`
		} `json:"subjects"`
	}
	require.NoError(t, json.Unmarshal([]byte(docs[1]), &binding))
	assert.Equal(t, ClientCN, binding.Subjects[0].Name)

	assert.Contains(t, docs[2], `"failurePolicy": "Ignore"`)
}

func TestRenderManifests_IsDeterministic(t *testing.T) {
	w := WebhookConfig{Name: "n", URL: "https://127.0.0.1:8444/mutate", CA: []byte("ca")}
	a, err := RenderManifests(w)
	require.NoError(t, err)
	b, err := RenderManifests(w)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

func TestParseServiceConfig_DefaultsFromTheNodeEnvironment(t *testing.T) {
	env := map[string]string{"EKS_REGION": "ap-southeast-2", "EKS_ADDON_GATEWAY_URL": "https://gw:9999", "EKS_GATEWAY_CA": "/etc/gw.pem"}
	c, err := ParseServiceConfig(nil, func(k string) string { return env[k] }, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "ap-southeast-2", c.Region)
	assert.Equal(t, "https://gw:9999", c.STSEndpoint)
	assert.Equal(t, "/etc/gw.pem", c.GatewayCAFile)
	assert.Equal(t, DefaultAddr, c.Addr)
}

func TestParseServiceConfig_RejectsANonLoopbackListener(t *testing.T) {
	vars := map[string]string{"EKS_REGION": "r", "EKS_ADDON_GATEWAY_URL": "https://gw"}
	env := func(k string) string { return vars[k] }
	for _, addr := range []string{"0.0.0.0:8444", ":8444", "10.0.0.5:8444", "somehost:8444"} {
		_, err := ParseServiceConfig([]string{"--addr=" + addr}, env, &bytes.Buffer{})
		assert.Error(t, err, addr)
	}
	_, err := ParseServiceConfig([]string{"--addr=127.0.0.1:9000"}, env, &bytes.Buffer{})
	assert.NoError(t, err)
}

func TestParseServiceConfig_RequiresRegionAndSTSEndpoint(t *testing.T) {
	_, err := ParseServiceConfig(nil, func(string) string { return "" }, &bytes.Buffer{})
	assert.Error(t, err)
}

func TestStageManifests_WritesTheServerCAAndIsIdempotent(t *testing.T) {
	cfg, tls := certTestConfig(t)
	c := ServiceConfig{Addr: DefaultAddr, TLSDir: tls, ManifestDir: t.TempDir()}
	require.NoError(t, c.stageManifests())

	path := filepath.Join(c.ManifestDir, manifestFile)
	first, err := os.ReadFile(path)
	require.NoError(t, err)
	ca, err := os.ReadFile(cfg.ServerCACert)
	require.NoError(t, err)
	assert.Contains(t, string(first), base64Of(ca), "caBundle must be the k3s server CA")
	assert.Contains(t, string(first), "https://127.0.0.1:8444/mutate")

	st1, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, c.stageManifests())
	st2, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, st1.ModTime().Equal(st2.ModTime()), "an unchanged manifest must not be rewritten")
}

func TestCABundle_IsSystemRootsPlusGatewayCA(t *testing.T) {
	dir := t.TempDir()
	writeCA(t, dir, "sys")
	writeCA(t, dir, "gw")
	c := ServiceConfig{SystemRoots: filepath.Join(dir, "sys.crt"), GatewayCAFile: filepath.Join(dir, "gw.crt")}
	b, err := c.caBundle()
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(b))
	assert.Len(t, pool.Subjects(), 2) //nolint:staticcheck // counting entries only
}

func base64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestEnsureCerts_AreValidForOneYear(t *testing.T) {
	cfg, _ := certTestConfig(t)
	out, err := EnsureCerts(cfg)
	require.NoError(t, err)
	for _, p := range []string{out.ServingCert, out.ClientCert} {
		c := parseCert(t, p)
		assert.InDelta(t, 365*24, c.NotAfter.Sub(c.NotBefore).Hours()-1, 24, p)
		assert.Less(t, time.Until(c.NotAfter), 366*24*time.Hour, p)
	}
}

func TestNeedsRenewal(t *testing.T) {
	cfg, _ := certTestConfig(t)
	out, err := EnsureCerts(cfg)
	require.NoError(t, err)
	now := time.Now()

	assert.False(t, NeedsRenewal(out, now), "fresh certs are outside the window")
	assert.False(t, NeedsRenewal(out, now.Add(300*24*time.Hour)))
	assert.True(t, NeedsRenewal(out, now.Add(340*24*time.Hour)), "inside 30 days of expiry")
	assert.True(t, NeedsRenewal(out, now.Add(400*24*time.Hour)), "already expired")

	require.NoError(t, os.Remove(out.ClientCert))
	assert.True(t, NeedsRenewal(out, now), "an unreadable certificate needs renewal")
}

func TestEnsureCerts_ReissuesACertInsideTheRenewWindow(t *testing.T) {
	cfg, _ := certTestConfig(t)
	out, err := EnsureCerts(cfg)
	require.NoError(t, err)
	// Replace the serving cert with one that expires in 10 days.
	ca, caKey, err := konnectivitycert.LoadCA(cfg.ServerCACert, cfg.ServerCAKey)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "pod-identity-webhook"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	kd, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(out.ServingKey, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600))
	require.NoError(t, os.WriteFile(out.ServingCert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	require.True(t, NeedsRenewal(out, time.Now()))

	_, err = EnsureCerts(cfg)
	require.NoError(t, err)
	assert.False(t, NeedsRenewal(out, time.Now()), "the restart path must leave certs outside the window")
}
