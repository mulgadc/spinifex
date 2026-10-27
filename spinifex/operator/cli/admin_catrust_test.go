package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/host/hostos"
)

func TestTrustCACertificate(t *testing.T) {
	src := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(src, []byte("CA PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		update    string
		anchor    string
		updateErr error
		wantErr   string
	}{
		"Debian trust store": {update: "update-ca-certificates", anchor: "usr/local/share/ca-certificates"},
		"EL trust store":     {update: "update-ca-trust", anchor: "etc/pki/ca-trust/source/anchors"},
		"update fails":       {update: "update-ca-trust", anchor: "anchors", updateErr: errors.New("exit status 1"), wantErr: "update-ca-trust failed"},
	} {
		t.Run(name, func(t *testing.T) {
			trust := hostos.CATrust{
				Update:    hostos.Tool{Name: tc.update, Path: "/usr/bin/" + tc.update},
				AnchorDir: filepath.Join(t.TempDir(), tc.anchor),
			}
			var ran string
			err := trustCACertificate(src, trust, func(path string) error { ran = path; return tc.updateErr })
			if ran != trust.Update.Path {
				t.Errorf("ran %q, want %q", ran, trust.Update.Path)
			}
			got, readErr := os.ReadFile(filepath.Join(trust.AnchorDir, caTrustFile))
			if readErr != nil || string(got) != "CA PEM" {
				t.Errorf("anchor file = %q, %v; want the CA written before the update runs", got, readErr)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("trustCACertificate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestTrustCACertificate_UnreadableSourceRunsNoUpdate(t *testing.T) {
	trust := hostos.CATrust{Update: hostos.Tool{Name: "update-ca-trust", Path: "/usr/bin/update-ca-trust"}, AnchorDir: t.TempDir()}
	err := trustCACertificate(filepath.Join(t.TempDir(), "missing.pem"), trust, func(string) error {
		t.Error("update ran without a CA to trust")
		return nil
	})
	if err == nil {
		t.Fatal("expected an error for a missing CA certificate")
	}
}
