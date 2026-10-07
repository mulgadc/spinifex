package webhookcert

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsIncompleteConfig(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no dir", Config{CN: testCN, DNS: testCN}, "-dir, -cn and -dns are required"},
		{"no cn", Config{Dir: dir, DNS: testCN}, "-dir, -cn and -dns are required"},
		{"no dns", Config{Dir: dir, CN: testCN}, "-dir, -cn and -dns are required"},
		{"blank dns list", Config{Dir: dir, CN: testCN, DNS: " , ,"}, "-dns lists no names"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			tc.cfg.Out = &out
			err := Run(context.Background(), tc.cfg)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Run error = %v, want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("Run wrote %q on failure", out.String())
			}
		})
	}
}

func TestRunReportsCacheDirFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "certs")
	var out bytes.Buffer
	err := Run(context.Background(), Config{Dir: dir, CN: testCN, DNS: testCN, Out: &out})
	if err == nil || !strings.HasPrefix(err.Error(), "mkdir "+dir+": ") {
		t.Fatalf("Run error = %v, want mkdir failure for %s", err, dir)
	}
	if out.Len() != 0 {
		t.Errorf("Run wrote %q on failure", out.String())
	}
}

func TestRunPrintsCachedPairAsCABundleLeafAndKey(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := Run(context.Background(), Config{Dir: dir, CN: testCN, DNS: testCN + "," + testDNS, Out: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	line, ok := strings.CutSuffix(out.String(), "\n")
	if !ok || strings.Contains(line, "\n") {
		t.Fatalf("output %q is not exactly one line", out.String())
	}
	fields := strings.Split(line, "\t")
	if len(fields) != 3 {
		t.Fatalf("got %d fields, want ca, crt, key", len(fields))
	}
	if fields[0] != fields[1] {
		t.Error("caBundle and serving cert differ; the apiserver would not trust the webhook")
	}
	for i, name := range map[int]string{1: "tls.crt", 2: "tls.key"} {
		onDisk, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fields[i] != base64.StdEncoding.EncodeToString(onDisk) {
			t.Errorf("field %d does not encode the cached %s", i, name)
		}
	}
	leaf := parseLeaf(t, onDiskCert(t, dir))
	if err := leaf.VerifyHostname(testDNS); err != nil {
		t.Errorf("SAN from -dns missing: %v", err)
	}
}

func onDiskCert(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
