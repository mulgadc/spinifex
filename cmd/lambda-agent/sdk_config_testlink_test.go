//go:build lambdatestlink

//test:in-package exercises the unexported boot configuration reader
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSDKConfig_OptionalAndInvalid(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	for _, tc := range []struct {
		name     string
		endpoint []byte
		ca       []byte
		wantErr  bool
		want     bool
	}{
		{name: "absent"},
		{name: "valid", endpoint: []byte(server.URL), ca: ca, want: true},
		{name: "missing ca", endpoint: []byte(server.URL), wantErr: true},
		{name: "missing endpoint", ca: ca, wantErr: true},
		{name: "invalid endpoint", endpoint: []byte("http://127.0.0.1:1234"), ca: ca, wantErr: true},
		{name: "invalid ca", endpoint: []byte(server.URL), ca: []byte("not a certificate"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, value := range map[string][]byte{sdkEndpointEntry: tc.endpoint, sdkCAEntry: tc.ca} {
				if value != nil {
					require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0o700))
					require.NoError(t, os.WriteFile(filepath.Join(dir, name, "raw"), value, 0o600))
				}
			}
			cfg, err := readSDKConfig(dir)
			if tc.wantErr {
				require.Error(t, err)
				require.Nil(t, cfg)
				return
			}
			require.NoError(t, err)
			if !tc.want {
				require.Nil(t, cfg)
				return
			}
			bundle := filepath.Join(t.TempDir(), "sdk", "ca.pem")
			require.NoError(t, cfg.writeBundle(bundle, filepath.Join(t.TempDir(), "absent")))
			data, err := os.ReadFile(bundle)
			require.NoError(t, err)
			roots := x509.NewCertPool()
			require.True(t, roots.AppendCertsFromPEM(data))
			client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
			resp, err := client.Get(cfg.endpoint)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
}

func TestSDKConfig_BundleKeepsSystemRoots(t *testing.T) {
	cluster := httptest.NewTLSServer(nil)
	defer cluster.Close()
	clusterCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.Certificate().Raw})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test system root"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	systemCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	dir := t.TempDir()
	systemPath, bundlePath := filepath.Join(dir, "system.pem"), filepath.Join(dir, "sdk", "ca.pem")
	require.NoError(t, os.WriteFile(systemPath, systemCA, 0o600))
	require.NoError(t, (&sdkConfig{ca: clusterCA}).writeBundle(bundlePath, systemPath))
	bundle, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(bundle))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	resp, err := client.Get(cluster.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	systemCert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	_, err = systemCert.Verify(x509.VerifyOptions{Roots: roots})
	require.NoError(t, err)
}
