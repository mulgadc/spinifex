package admin

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Key / Token generation ---

func TestGenerateAWSAccessKey_Format(t *testing.T) {
	key, err := GenerateAWSAccessKey()
	assert.NoError(t, err)
	assert.Len(t, key, 20)
	assert.True(t, strings.HasPrefix(key, "AKIA"))
	for _, c := range key[4:] {
		assert.True(t, (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'),
			"unexpected character %c in access key suffix", c)
	}
}

func TestGenerateAWSAccessKey_Uniqueness(t *testing.T) {
	k1, err := GenerateAWSAccessKey()
	assert.NoError(t, err)
	k2, err := GenerateAWSAccessKey()
	assert.NoError(t, err)
	assert.NotEqual(t, k1, k2)
}

func TestGenerateAWSSecretKey_Format(t *testing.T) {
	key, err := GenerateAWSSecretKey()
	assert.NoError(t, err)
	assert.Len(t, key, 40)
	_, err = base64.StdEncoding.DecodeString(key)
	assert.NoError(t, err, "secret key should be valid base64")
}

func TestGenerateAWSSecretKey_Uniqueness(t *testing.T) {
	k1, err := GenerateAWSSecretKey()
	assert.NoError(t, err)
	k2, err := GenerateAWSSecretKey()
	assert.NoError(t, err)
	assert.NotEqual(t, k1, k2)
}

func TestSystemAccountID(t *testing.T) {
	id := SystemAccountID()
	assert.Equal(t, "000000000000", id)
	assert.Len(t, id, 12)
}

func TestDefaultAccountID(t *testing.T) {
	id := DefaultAccountID()
	assert.Equal(t, "000000000001", id)
	assert.Len(t, id, 12)
}

func TestGenerateNATSToken_Format(t *testing.T) {
	token, err := GenerateNATSToken()
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(token, "nats_"))
	assert.Len(t, token, 37) // 5 prefix + 32 random
	// URL-safe base64: no '+' or '/'
	assert.NotContains(t, token, "+")
	assert.NotContains(t, token, "/")
}

func TestGenerateNATSToken_Uniqueness(t *testing.T) {
	t1, err := GenerateNATSToken()
	assert.NoError(t, err)
	t2, err := GenerateNATSToken()
	assert.NoError(t, err)
	assert.NotEqual(t, t1, t2)
}

// --- Config file generation ---

func TestGenerateConfigFile_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.conf")

	tmpl := "region = {{.Region}}\nnode = {{.Node}}"
	settings := ConfigSettings{Region: "us-east-1", Node: "node1"}

	err := GenerateConfigFile(path, tmpl, settings)
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "region = us-east-1")
	assert.Contains(t, string(data), "node = node1")

	info, _ := os.Stat(path)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestGenerateConfigFile_InvalidTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.conf")
	err := GenerateConfigFile(path, "{{.Unclosed", ConfigSettings{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse template")
}

func TestGenerateConfigFile_InvalidPath(t *testing.T) {
	err := GenerateConfigFile("/nonexistent/dir/file.conf", "ok", ConfigSettings{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create config file")
}

func TestGenerateConfigFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overwrite.conf")

	require.NoError(t, GenerateConfigFile(path, "old={{.Region}}", ConfigSettings{Region: "old"}))
	require.NoError(t, GenerateConfigFile(path, "new={{.Region}}", ConfigSettings{Region: "new"}))

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "new=new")
	assert.NotContains(t, string(data), "old")
}

func TestGenerateConfigFiles_AllSucceed(t *testing.T) {
	dir := t.TempDir()
	configs := []ConfigFile{
		{Name: "a", Path: filepath.Join(dir, "a.conf"), Template: "a={{.Region}}"},
		{Name: "b", Path: filepath.Join(dir, "b.conf"), Template: "b={{.Node}}"},
	}
	err := GenerateConfigFiles(configs, ConfigSettings{Region: "us-west-2", Node: "n1"})
	require.NoError(t, err)

	for _, cfg := range configs {
		assert.True(t, FileExists(cfg.Path))
	}
}

func TestGenerateConfigFiles_StopsOnFirstError(t *testing.T) {
	dir := t.TempDir()
	configs := []ConfigFile{
		{Name: "ok", Path: filepath.Join(dir, "ok.conf"), Template: "ok"},
		{Name: "bad", Path: "/nonexistent/dir/bad.conf", Template: "bad"},
		{Name: "never", Path: filepath.Join(dir, "never.conf"), Template: "never"},
	}
	err := GenerateConfigFiles(configs, ConfigSettings{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "bad")
	assert.False(t, FileExists(filepath.Join(dir, "never.conf")))
}

// --- FileExists ---

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	require.NoError(t, os.WriteFile(existing, []byte("hi"), 0644))

	assert.True(t, FileExists(existing))
	assert.False(t, FileExists(filepath.Join(dir, "nope.txt")))
	assert.True(t, FileExists(dir)) // directory also returns true
}

// --- UpdateAWSINIFile ---

func TestUpdateAWSINIFile_CreateNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	err := UpdateAWSINIFile(path, "spinifex", map[string]string{
		"aws_access_key_id":     "AKIATEST",
		"aws_secret_access_key": "secrettest",
	})
	require.NoError(t, err)

	data, _ := os.ReadFile(path)
	content := string(data)
	assert.Contains(t, content, "[spinifex]")
	assert.Contains(t, content, "AKIATEST")
	assert.Contains(t, content, "secrettest")
}

func TestUpdateAWSINIFile_UpdateExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	// Create with initial value
	require.NoError(t, UpdateAWSINIFile(path, "spinifex", map[string]string{"key": "old"}))
	// Update
	require.NoError(t, UpdateAWSINIFile(path, "spinifex", map[string]string{"key": "new"}))

	data, _ := os.ReadFile(path)
	content := string(data)
	assert.Contains(t, content, "new")
}

func TestUpdateAWSINIFile_AddNewSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	require.NoError(t, UpdateAWSINIFile(path, "default", map[string]string{"key": "default-val"}))
	require.NoError(t, UpdateAWSINIFile(path, "spinifex", map[string]string{"key": "spinifex-val"}))

	data, _ := os.ReadFile(path)
	content := string(data)
	assert.Contains(t, content, "[default]")
	assert.Contains(t, content, "[spinifex]")
	assert.Contains(t, content, "default-val")
	assert.Contains(t, content, "spinifex-val")
}

// --- SetupAWSCredentials ---

func TestSetupAWSCredentials_CreatesFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	err := SetupAWSCredentials("AKIATEST123", "secret123", "us-east-1", "/path/to/ca.pem", "", "")
	require.NoError(t, err)

	credData, _ := os.ReadFile(filepath.Join(dir, ".aws", "credentials"))
	configData, _ := os.ReadFile(filepath.Join(dir, ".aws", "config"))

	assert.Contains(t, string(credData), "AKIATEST123")
	assert.Contains(t, string(credData), "secret123")
	assert.Contains(t, string(configData), "us-east-1")
	assert.Contains(t, string(configData), "https://localhost:9999")
	assert.Contains(t, string(configData), "/path/to/ca.pem")
}

func TestSetupAWSCredentials_PreservesExistingProfiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	awsDir := filepath.Join(dir, ".aws")
	require.NoError(t, os.MkdirAll(awsDir, 0700))
	require.NoError(t, UpdateAWSINIFile(filepath.Join(awsDir, "credentials"), "default", map[string]string{
		"aws_access_key_id": "EXISTING_KEY",
	}))

	err := SetupAWSCredentials("NEWAKEY", "NEWSECRET", "us-west-2", "/ca.pem", "", "")
	require.NoError(t, err)

	data, _ := os.ReadFile(filepath.Join(awsDir, "credentials"))
	content := string(data)
	assert.Contains(t, content, "EXISTING_KEY")
	assert.Contains(t, content, "NEWAKEY")
}

func TestSetupAWSCredentials_UsesBindIP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	err := SetupAWSCredentials("AKIATEST123", "secret123", "us-east-1", "/ca.pem", "10.11.12.1", "")
	require.NoError(t, err)

	configData, _ := os.ReadFile(filepath.Join(dir, ".aws", "config"))
	assert.Contains(t, string(configData), "https://10.11.12.1:9999")
	assert.NotContains(t, string(configData), "localhost")
}

func TestSetupAWSCredentials_FallsBackToLocalhostForWildcard(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	err := SetupAWSCredentials("AKIATEST123", "secret123", "us-east-1", "/ca.pem", "0.0.0.0", "")
	require.NoError(t, err)

	configData, _ := os.ReadFile(filepath.Join(dir, ".aws", "config"))
	assert.Contains(t, string(configData), "https://localhost:9999")
}

// --- Certificate generation ---

func TestGenerateCACert_CreatesValidCA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca.key")

	err := GenerateCACert(certPath, keyPath)
	require.NoError(t, err)

	// Parse certificate
	certPEM, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	assert.Equal(t, "CERTIFICATE", block.Type)

	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.True(t, cert.IsCA)
	assert.Equal(t, "Spinifex Local CA", cert.Subject.CommonName)
	assert.NotZero(t, cert.KeyUsage&x509.KeyUsageCertSign)

	// Parse key
	keyPEM, _ := os.ReadFile(keyPath)
	keyBlock, _ := pem.Decode(keyPEM)
	require.NotNil(t, keyBlock)
	assert.Equal(t, "PRIVATE KEY", keyBlock.Type)

	// Verify key file permissions
	info, _ := os.Stat(keyPath)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

// TestGenerateSignedCert uses subtests sharing a single CA to avoid repeated 4096-bit key generation.
func TestGenerateSignedCert(t *testing.T) {
	t.Parallel()
	// Generate CA once for all subtests (~0.7s instead of ~0.7s x 3)
	caDir := t.TempDir()
	caCertPath := filepath.Join(caDir, "ca.pem")
	caKeyPath := filepath.Join(caDir, "ca.key")
	require.NoError(t, GenerateCACert(caCertPath, caKeyPath))

	t.Run("CreatesValidCert", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		certPath := filepath.Join(dir, "server.pem")
		keyPath := filepath.Join(dir, "server.key")

		require.NoError(t, GenerateSignedCert(certPath, keyPath, caCertPath, caKeyPath))

		certPEM, _ := os.ReadFile(certPath)
		block, _ := pem.Decode(certPEM)
		require.NotNil(t, block)

		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		assert.False(t, cert.IsCA)
		assert.Equal(t, "Spinifex Server", cert.Subject.CommonName)
		assert.Contains(t, cert.DNSNames, "localhost")

		hasLoopback := false
		for _, ip := range cert.IPAddresses {
			if ip.Equal(net.ParseIP("127.0.0.1")) {
				hasLoopback = true
			}
		}
		assert.True(t, hasLoopback)

		// Auto-discovered IPs should be present (at least loopback + any interface IPs).
		assert.GreaterOrEqual(t, len(cert.IPAddresses), 2)

		// Verify against CA
		caCertPEM, _ := os.ReadFile(caCertPath)
		caBlock, _ := pem.Decode(caCertPEM)
		caCert, _ := x509.ParseCertificate(caBlock.Bytes)
		pool := x509.NewCertPool()
		pool.AddCert(caCert)
		_, err = cert.Verify(x509.VerifyOptions{Roots: pool})
		assert.NoError(t, err)

		info, _ := os.Stat(keyPath)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	})

	t.Run("ExtraIPs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		certPath := filepath.Join(dir, "server.pem")
		keyPath := filepath.Join(dir, "server.key")

		require.NoError(t, GenerateSignedCert(certPath, keyPath, caCertPath, caKeyPath, "192.168.1.100"))

		certPEM, _ := os.ReadFile(certPath)
		block, _ := pem.Decode(certPEM)
		cert, _ := x509.ParseCertificate(block.Bytes)

		hasExtraIP := false
		for _, ip := range cert.IPAddresses {
			if ip.Equal(net.ParseIP("192.168.1.100")) {
				hasExtraIP = true
			}
		}
		assert.True(t, hasExtraIP, "cert should contain extra IP 192.168.1.100")
	})

	t.Run("SkipsDuplicateAndSpecialIPs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		certPath := filepath.Join(dir, "server.pem")
		keyPath := filepath.Join(dir, "server.key")

		require.NoError(t, GenerateSignedCert(certPath, keyPath, caCertPath, caKeyPath, "127.0.0.1", "::1", "0.0.0.0", ""))

		certPEM, _ := os.ReadFile(certPath)
		block, _ := pem.Decode(certPEM)
		cert, _ := x509.ParseCertificate(block.Bytes)

		// Should have loopback (127.0.0.1, ::1) + auto-discovered interface IPs.
		// Duplicates (127.0.0.1, ::1) and specials (0.0.0.0, "") must not add extras.
		// Count unique IPs from a clean run without extras.
		baseDir := t.TempDir()
		baseCert := filepath.Join(baseDir, "server.pem")
		baseKey := filepath.Join(baseDir, "server.key")
		require.NoError(t, GenerateSignedCert(baseCert, baseKey, caCertPath, caKeyPath))
		basePEM, _ := os.ReadFile(baseCert)
		baseBlock, _ := pem.Decode(basePEM)
		baseParsed, _ := x509.ParseCertificate(baseBlock.Bytes)

		assert.Equal(t, len(baseParsed.IPAddresses), len(cert.IPAddresses),
			"passing duplicate/special IPs should not add extra entries")
	})

	t.Run("InvalidCACert", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		badCACert := filepath.Join(dir, "bad-ca.pem")
		require.NoError(t, os.WriteFile(badCACert, []byte("not-a-cert"), 0600))

		err := GenerateSignedCert(filepath.Join(dir, "s.pem"), filepath.Join(dir, "s.key"), badCACert, caKeyPath)
		assert.Error(t, err)
	})
}

func TestDiscoverLocalIPs(t *testing.T) {
	t.Parallel()
	ips := DiscoverLocalIPs()
	// Should find at least one non-loopback IP on any machine with a network interface.
	assert.NotEmpty(t, ips, "expected at least one non-loopback IP")

	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		require.NotNil(t, parsed, "returned IP should be parseable: %s", ip)
		assert.False(t, parsed.IsLoopback(), "should not include loopback: %s", ip)
		assert.False(t, parsed.IsLinkLocalUnicast(), "should not include link-local: %s", ip)
	}

	// No duplicates.
	seen := make(map[string]struct{})
	for _, ip := range ips {
		_, dup := seen[ip]
		assert.False(t, dup, "duplicate IP: %s", ip)
		seen[ip] = struct{}{}
	}
}

func TestDiscoverHostname(t *testing.T) {
	t.Parallel()
	hostname := DiscoverHostname()
	// We can't assert a specific value, but if non-empty it shouldn't be "localhost".
	if hostname != "" {
		assert.NotEqual(t, "localhost", hostname)
	}
}

func TestGenerateSignedCertWithDNS_DedupDNS(t *testing.T) {
	t.Parallel()
	caDir := t.TempDir()
	caCertPath := filepath.Join(caDir, "ca.pem")
	caKeyPath := filepath.Join(caDir, "ca.key")
	require.NoError(t, GenerateCACert(caCertPath, caKeyPath))

	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")

	// Pass "localhost" as extra DNS — should not duplicate the built-in entry.
	// Also pass empty strings and duplicates — should be ignored.
	extraDNS := []string{"localhost", "", "example.local", "example.local", ""}
	require.NoError(t, GenerateSignedCertWithDNS(certPath, keyPath, caCertPath, caKeyPath, nil, extraDNS))

	certPEM, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)

	// Count occurrences of "localhost" — must be exactly 1.
	localhostCount := 0
	exampleCount := 0
	for _, dns := range cert.DNSNames {
		if dns == "localhost" {
			localhostCount++
		}
		if dns == "example.local" {
			exampleCount++
		}
	}
	assert.Equal(t, 1, localhostCount, "localhost should appear exactly once")
	assert.Equal(t, 1, exampleCount, "example.local should appear exactly once")
	assert.Contains(t, cert.DNSNames, "example.local")
}

func TestGenerateSignedCertWithDNS_NilSlices(t *testing.T) {
	t.Parallel()
	caDir := t.TempDir()
	caCertPath := filepath.Join(caDir, "ca.pem")
	caKeyPath := filepath.Join(caDir, "ca.key")
	require.NoError(t, GenerateCACert(caCertPath, caKeyPath))

	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")

	// Both slices nil — should still produce a valid cert with defaults.
	require.NoError(t, GenerateSignedCertWithDNS(certPath, keyPath, caCertPath, caKeyPath, nil, nil))

	certPEM, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)

	assert.Contains(t, cert.DNSNames, "localhost")
	assert.GreaterOrEqual(t, len(cert.IPAddresses), 2, "should have at least loopback IPs")
}

func TestGenerateSignedCertWithDNS_InvalidCA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Missing CA cert file.
	err := GenerateSignedCertWithDNS(
		filepath.Join(dir, "s.pem"), filepath.Join(dir, "s.key"),
		filepath.Join(dir, "missing-ca.pem"), filepath.Join(dir, "missing-ca.key"),
		nil, nil,
	)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read CA cert")

	// Corrupt CA cert.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad-ca.pem"), []byte("not-pem"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad-ca.key"), []byte("not-pem"), 0600))
	err = GenerateSignedCertWithDNS(
		filepath.Join(dir, "s.pem"), filepath.Join(dir, "s.key"),
		filepath.Join(dir, "bad-ca.pem"), filepath.Join(dir, "bad-ca.key"),
		nil, nil,
	)
	assert.Error(t, err)
}

func TestGenerateSignedCertWithDNS_ExtraDNS(t *testing.T) {
	t.Parallel()
	caDir := t.TempDir()
	caCertPath := filepath.Join(caDir, "ca.pem")
	caKeyPath := filepath.Join(caDir, "ca.key")
	require.NoError(t, GenerateCACert(caCertPath, caKeyPath))

	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")

	extraIPs := []string{"10.0.0.42"}
	extraDNS := []string{"spinifex.local", "node1.spinifex.local"}
	require.NoError(t, GenerateSignedCertWithDNS(certPath, keyPath, caCertPath, caKeyPath, extraIPs, extraDNS))

	certPEM, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)

	assert.Contains(t, cert.DNSNames, "localhost")
	assert.Contains(t, cert.DNSNames, "spinifex.local")
	assert.Contains(t, cert.DNSNames, "node1.spinifex.local")

	hasExtraIP := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.ParseIP("10.0.0.42")) {
			hasExtraIP = true
		}
	}
	assert.True(t, hasExtraIP, "cert should contain extra IP 10.0.0.42")

	info, _ := os.Stat(keyPath)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestGenerateSignedCert_IncludesHostname(t *testing.T) {
	t.Parallel()
	caDir := t.TempDir()
	caCertPath := filepath.Join(caDir, "ca.pem")
	caKeyPath := filepath.Join(caDir, "ca.key")
	require.NoError(t, GenerateCACert(caCertPath, caKeyPath))

	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")
	require.NoError(t, GenerateSignedCert(certPath, keyPath, caCertPath, caKeyPath))

	certPEM, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)

	assert.Contains(t, cert.DNSNames, "localhost")
	hostname := DiscoverHostname()
	if hostname != "" {
		assert.Contains(t, cert.DNSNames, hostname,
			"cert DNS SANs should include the machine hostname")
	}
}

func TestGenerateSelfSignedCert_CreatesValidCert(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "self.pem")
	keyPath := filepath.Join(dir, "self.key")

	require.NoError(t, GenerateSelfSignedCert(certPath, keyPath))

	certPEM, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)

	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Contains(t, cert.DNSNames, "localhost")

	info, _ := os.Stat(keyPath)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

// --- Certificate orchestrator ---

// TestGenerateCertificatesIfNeeded uses subtests to share the initial generation.
func TestGenerateCertificatesIfNeeded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// First call generates all certs
	caCertPath := GenerateCertificatesIfNeeded(dir, false, "")
	assert.Equal(t, filepath.Join(dir, "ca.pem"), caCertPath)
	assert.True(t, FileExists(filepath.Join(dir, "ca.pem")))
	assert.True(t, FileExists(filepath.Join(dir, "ca.key")))
	assert.True(t, FileExists(filepath.Join(dir, "server.pem")))
	assert.True(t, FileExists(filepath.Join(dir, "server.key")))

	t.Run("SkipsWhenAllExist", func(t *testing.T) {
		caInfo, _ := os.Stat(filepath.Join(dir, "ca.pem"))
		origModTime := caInfo.ModTime()

		GenerateCertificatesIfNeeded(dir, false, "")
		caInfo2, _ := os.Stat(filepath.Join(dir, "ca.pem"))
		assert.Equal(t, origModTime, caInfo2.ModTime())
	})

	t.Run("ForceRegenerates", func(t *testing.T) {
		origCA, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))

		GenerateCertificatesIfNeeded(dir, true, "")
		newCA, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
		assert.NotEqual(t, origCA, newCA)
	})
}

func TestGenerateServerCertOnly(t *testing.T) {
	t.Parallel()
	// Generate CA once, reuse for subtests
	caDir := t.TempDir()
	require.NoError(t, GenerateCACert(filepath.Join(caDir, "ca.pem"), filepath.Join(caDir, "ca.key")))

	t.Run("Success", func(t *testing.T) {
		dir := t.TempDir()
		// Copy CA files into test dir
		caCert, _ := os.ReadFile(filepath.Join(caDir, "ca.pem"))
		caKey, _ := os.ReadFile(filepath.Join(caDir, "ca.key"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.pem"), caCert, 0600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.key"), caKey, 0600))

		err := GenerateServerCertOnly(dir, "10.0.0.5")
		require.NoError(t, err)
		assert.True(t, FileExists(filepath.Join(dir, "server.pem")))
		assert.True(t, FileExists(filepath.Join(dir, "server.key")))

		certPEM, _ := os.ReadFile(filepath.Join(dir, "server.pem"))
		block, _ := pem.Decode(certPEM)
		cert, _ := x509.ParseCertificate(block.Bytes)

		hasBindIP := false
		for _, ip := range cert.IPAddresses {
			if ip.Equal(net.ParseIP("10.0.0.5")) {
				hasBindIP = true
			}
		}
		assert.True(t, hasBindIP)
	})

	t.Run("MissingCA", func(t *testing.T) {
		dir := t.TempDir()
		err := GenerateServerCertOnly(dir, "10.0.0.5")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "CA files not found")
	})
}

// --- Directory creation ---

func TestCreateServiceDirectories_CreatesAll(t *testing.T) {
	dir := t.TempDir()
	CreateServiceDirectories(dir)

	expected := []string{"images", "amis", "volumes", "state", "logs", "nats", "predastore", "viperblock", "spinifex"}
	for _, name := range expected {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		assert.NoError(t, err, "directory %s should exist", name)
		if err == nil {
			assert.True(t, info.IsDir())
		}
	}
}

func TestCreateServiceDirectories_Idempotent(t *testing.T) {
	dir := t.TempDir()
	CreateServiceDirectories(dir)
	// Should not error on second call
	CreateServiceDirectories(dir)

	info, err := os.Stat(filepath.Join(dir, "images"))
	assert.NoError(t, err)
	assert.True(t, info.IsDir())
}

// --- Predastore multi-node config ---

func TestGenerateMultiNodePredastoreConfig_Success(t *testing.T) {
	tmpl := `{{range .Nodes}}[[db]]
id = {{.ID}}
host = "{{.Host}}"
{{end}}`
	nodes := []PredastoreNodeConfig{
		{ID: 1, Host: "10.0.0.1"},
		{ID: 2, Host: "10.0.0.2"},
		{ID: 3, Host: "10.0.0.3"},
	}

	result, err := GenerateMultiNodePredastoreConfig(tmpl, nodes, "AK", "SK", "us-east-1", "nats-token", "/config", "10.0.0.1")
	require.NoError(t, err)
	assert.Contains(t, result, `host = "10.0.0.1"`)
	assert.Contains(t, result, `host = "10.0.0.3"`)
}

func TestGenerateMultiNodePredastoreConfig_MinimumNodes(t *testing.T) {
	tmpl := "{{range .Nodes}}{{.ID}}{{end}}"

	_, err := GenerateMultiNodePredastoreConfig(tmpl, []PredastoreNodeConfig{
		{ID: 1, Host: "10.0.0.1"},
		{ID: 2, Host: "10.0.0.2"},
	}, "AK", "SK", "us-east-1", "nats-token", "/config", "10.0.0.1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at least 3 nodes")
}

func TestGenerateMultiNodePredastoreConfig_InvalidTemplate(t *testing.T) {
	_, err := GenerateMultiNodePredastoreConfig("{{.Unclosed", []PredastoreNodeConfig{
		{ID: 1, Host: "a"}, {ID: 2, Host: "b"}, {ID: 3, Host: "c"},
	}, "AK", "SK", "us-east-1", "nats-token", "/config", "10.0.0.1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse")
}

// --- FindNodeIDByIP ---

func TestFindNodeIDByIP(t *testing.T) {
	nodes := []PredastoreNodeConfig{
		{ID: 1, Host: "10.0.0.1"},
		{ID: 2, Host: "10.0.0.2"},
		{ID: 3, Host: "10.0.0.3"},
	}

	assert.Equal(t, 2, FindNodeIDByIP(nodes, "10.0.0.2"))
	assert.Equal(t, 0, FindNodeIDByIP(nodes, "10.0.0.99"))
	assert.Equal(t, 0, FindNodeIDByIP(nil, "10.0.0.1"))
}

// --- ParsePredastoreNodeIDFromConfig ---

func TestParsePredastoreNodeIDFromConfig(t *testing.T) {
	tomlContent := `
[[db]]
id = 1
host = "10.0.0.1"

[[db]]
id = 2
host = "10.0.0.2"

[[db]]
id = 3
host = "10.0.0.3"
`
	assert.Equal(t, 2, ParsePredastoreNodeIDFromConfig(tomlContent, "10.0.0.2"))
	assert.Equal(t, 0, ParsePredastoreNodeIDFromConfig(tomlContent, "10.0.0.99"))
	assert.Equal(t, 0, ParsePredastoreNodeIDFromConfig("invalid toml {{{", "10.0.0.1"))
	assert.Equal(t, 0, ParsePredastoreNodeIDFromConfig("", "10.0.0.1"))
}

// --- Integration: Full config generation flow ---

func TestGenerateConfigFile_SpinifexTomlTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	tmpl := `version = "1.0"
epoch = 1
node = "{{.Node}}"

[nodes.{{.Node}}]
region = "{{.Region}}"
accesskey = "{{.AccessKey}}"
secretkey = "{{.SecretKey}}"
`

	settings := ConfigSettings{
		Node:      "node1",
		Region:    "us-east-1",
		AccessKey: "AKIATEST",
		SecretKey: "SECRET",
	}

	require.NoError(t, GenerateConfigFile(path, tmpl, settings))

	data, _ := os.ReadFile(path)
	content := string(data)
	assert.Contains(t, content, `node = "node1"`)
	assert.Contains(t, content, `region = "us-east-1"`)
	assert.Contains(t, content, fmt.Sprintf(`accesskey = "%s"`, settings.AccessKey))
}

func TestChownRecursive_InvalidUser(t *testing.T) {
	// ChownRecursive should silently return on invalid username
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	os.WriteFile(testFile, []byte("test"), 0644)

	// Should not panic or error with a non-existent user
	ChownRecursive(tmpDir, "nonexistent-user-that-does-not-exist-12345")

	// File should still exist and be readable
	_, err := os.ReadFile(testFile)
	assert.NoError(t, err)
}

func TestChownRecursive_CurrentUser(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "sub")
	os.MkdirAll(subDir, 0755)
	testFile := filepath.Join(subDir, "test.txt")
	os.WriteFile(testFile, []byte("test"), 0644)

	// Chown to current user should succeed (no-op effectively)
	currentUser := os.Getenv("USER")
	if currentUser == "" {
		t.Skip("USER env not set")
	}

	ChownRecursive(tmpDir, currentUser)

	// Verify files are still accessible
	_, err := os.ReadFile(testFile)
	assert.NoError(t, err)
}

func TestChownRecursive_NonExistentPath(t *testing.T) {
	// Should not panic on a path that doesn't exist
	currentUser := os.Getenv("USER")
	if currentUser == "" {
		t.Skip("USER env not set")
	}
	ChownRecursive("/tmp/nonexistent-path-12345", currentUser)
}

// --- SetGPUPassthrough ---

func writeToml(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "spinifex*.toml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func readToml(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func TestSetGPUPassthrough_NoOp(t *testing.T) {
	toml := "[nodes.node1.daemon]\ngpu_passthrough = true\n"
	path := writeToml(t, toml)
	require.NoError(t, SetGPUPassthrough(path, "node1", true))
	assert.Equal(t, toml, readToml(t, path))
}

func TestSetGPUPassthrough_Flip(t *testing.T) {
	path := writeToml(t, "[nodes.node1.daemon]\ngpu_passthrough = false\n")
	require.NoError(t, SetGPUPassthrough(path, "node1", true))
	assert.Contains(t, readToml(t, path), "gpu_passthrough = true")
}

func TestSetGPUPassthrough_AddKeyToSection(t *testing.T) {
	path := writeToml(t, "[nodes.node1.daemon]\nsome_other = true\n")
	require.NoError(t, SetGPUPassthrough(path, "node1", true))
	got := readToml(t, path)
	assert.Contains(t, got, "gpu_passthrough = true")
	assert.Contains(t, got, "some_other = true")
}

func TestSetGPUPassthrough_AppendSection(t *testing.T) {
	path := writeToml(t, "[nodes.node1.network]\ncidr = \"10.0.0.0/24\"\n")
	require.NoError(t, SetGPUPassthrough(path, "node1", true))
	got := readToml(t, path)
	assert.Contains(t, got, "[nodes.node1.daemon]")
	assert.Contains(t, got, "gpu_passthrough = true")
}

func TestSetGPUPassthrough_DisableNoOp(t *testing.T) {
	toml := "[nodes.node1.daemon]\ngpu_passthrough = false\n"
	path := writeToml(t, toml)
	require.NoError(t, SetGPUPassthrough(path, "node1", false))
	assert.Equal(t, toml, readToml(t, path))
}

func TestSetGPUPassthrough_ReadError(t *testing.T) {
	err := SetGPUPassthrough("/nonexistent/path/spinifex.toml", "node1", true)
	require.Error(t, err)
}

func TestSetGPUPassthrough_SectionBoundary(t *testing.T) {
	// gpu_passthrough = true exists but in a DIFFERENT node's section; should still write.
	path := writeToml(t, "[nodes.node2.daemon]\ngpu_passthrough = true\n[nodes.node1.daemon]\nsome_key = 1\n")
	require.NoError(t, SetGPUPassthrough(path, "node1", true))
	got := readToml(t, path)
	// node1 section should now have the key
	assert.Contains(t, got, "[nodes.node1.daemon]\ngpu_passthrough = true")
}
