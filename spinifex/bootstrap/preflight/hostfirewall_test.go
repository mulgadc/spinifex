package preflight

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCheckHostAt_HostFirewalls(t *testing.T) {
	for name, tc := range map[string]struct {
		active    []string
		ufwConf   string
		firewalld Status
		ufw       Status
		detail    string
	}{
		"neither installed":                 {firewalld: OK, ufw: OK},
		"firewalld active":                  {active: []string{"firewalld.service"}, firewalld: Failed, ufw: OK, detail: "systemctl mask --now firewalld"},
		"ufw unit active after ufw disable": {active: []string{"ufw.service"}, ufwConf: "ENABLED=no\n", firewalld: OK, ufw: OK},
		"ufw enabled":                       {active: []string{"ufw.service"}, ufwConf: "# /etc/ufw/ufw.conf\nENABLED=yes\nLOGLEVEL=low\n", firewalld: OK, ufw: Failed, detail: "ufw disable"},
		"ufw enabled, upper case":           {active: []string{"ufw.service"}, ufwConf: "ENABLED=YES\n", firewalld: OK, ufw: Failed, detail: "ufw disable"},
		"ufw enabled but its unit stopped":  {ufwConf: "ENABLED=yes\n", firewalld: OK, ufw: OK},
		"both active":                       {active: []string{"firewalld.service", "ufw.service"}, ufwConf: "ENABLED=yes\n", firewalld: Failed, ufw: Failed},
	} {
		t.Run(name, func(t *testing.T) {
			orig := unitActive
			t.Cleanup(func() { unitActive = orig })
			unitActive = func(unit string) bool { return slices.Contains(tc.active, unit) }
			root := t.TempDir()
			if tc.ufwConf != "" {
				path := filepath.Join(root, ufwConfig)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.ufwConf), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			results := CheckHostAt(root)
			for unit, want := range map[string]Status{"firewalld.service": tc.firewalld, "ufw.service": tc.ufw} {
				r := findResult(t, results, unit)
				if r.Status != want {
					t.Errorf("%s = %s %q, want %s", unit, r.Status, r.Detail, want)
				}
				if r.Status == Failed && !strings.Contains(r.Detail, tc.detail) {
					t.Errorf("%s detail %q does not name the fix %q", unit, r.Detail, tc.detail)
				}
			}
		})
	}
}
