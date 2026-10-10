package konnectivitycert

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsIncompleteConfig(t *testing.T) {
	dir := t.TempDir()
	full := Config{Dir: dir, CACert: "ca.crt", CAKey: "ca.key", CN: "konnectivity-server", SANs: "10.0.0.1"}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no dir", func(c *Config) { c.Dir = "" }, "-dir, -ca-cert, -ca-key and -sans are required"},
		{"no ca cert", func(c *Config) { c.CACert = "" }, "-dir, -ca-cert, -ca-key and -sans are required"},
		{"no ca key", func(c *Config) { c.CAKey = "" }, "-dir, -ca-cert, -ca-key and -sans are required"},
		{"no sans", func(c *Config) { c.SANs = "" }, "-dir, -ca-cert, -ca-key and -sans are required"},
		{"blank sans list", func(c *Config) { c.SANs = " , ," }, "-sans lists no usable names"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := full
			tc.mutate(&cfg)
			var out bytes.Buffer
			cfg.Out = &out
			err := Run(context.Background(), cfg)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Run error = %v, want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("Run wrote %q on failure", out.String())
			}
		})
	}
}

func TestRunReportsUnreadableCA(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.crt")
	var out bytes.Buffer
	err := Run(context.Background(), Config{
		Dir: filepath.Join(dir, "leaf"), CACert: missing, CAKey: missing, SANs: "10.0.0.1", Out: &out,
	})
	if err == nil || !strings.HasPrefix(err.Error(), "read ca cert "+missing+": ") {
		t.Fatalf("Run error = %v, want read ca cert failure", err)
	}
	if out.Len() != 0 {
		t.Errorf("Run wrote %q on failure", out.String())
	}
}

func TestRunPrintsCachedCertAndKeyPaths(t *testing.T) {
	caCert, caKey := writeTestCA(t, t.TempDir())
	dir := filepath.Join(t.TempDir(), "leaf")
	var out bytes.Buffer
	err := Run(context.Background(), Config{
		Dir: dir, CACert: caCert, CAKey: caKey, CN: "konnectivity-server", SANs: "10.32.100.4,eks.example", Out: &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := filepath.Join(dir, "tls.crt") + "\t" + filepath.Join(dir, "tls.key") + "\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
	leaf := leafFrom(t, filepath.Join(dir, "tls.crt"))
	if err := leaf.VerifyHostname("10.32.100.4"); err != nil {
		t.Errorf("IP SAN from -sans missing: %v", err)
	}
	if err := leaf.VerifyHostname("eks.example"); err != nil {
		t.Errorf("DNS SAN from -sans missing: %v", err)
	}
}
