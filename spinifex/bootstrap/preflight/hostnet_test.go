package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckHostAt_Hostnet(t *testing.T) {
	for name, tc := range map[string]struct {
		backend string
		state   bool
		want    Status
		detail  string
	}{
		"no record, nothing set up":  {want: OK},
		"networkd backend":           {backend: "networkd", want: OK},
		"hostnet backend, no state":  {backend: "hostnet", want: Failed, detail: "hostnet.json"},
		"hostnet backend, no marker": {backend: "hostnet", state: true, want: Failed, detail: "host network not applied"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			etc := filepath.Join(root, "etc/spinifex")
			if err := os.MkdirAll(etc, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.backend != "" {
				if err := os.WriteFile(filepath.Join(etc, "hostnet-backend"), []byte(tc.backend+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.state {
				state := `{"version": 1, "pending": false, "int_bridge": "br-int"}`
				if err := os.WriteFile(filepath.Join(etc, "hostnet.json"), []byte(state), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r := findResult(t, CheckHostAt(root), hostnetUnit)
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("hostnet result = %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
}
