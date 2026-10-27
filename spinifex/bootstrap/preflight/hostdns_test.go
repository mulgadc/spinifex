package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckHostAt_HostDNS(t *testing.T) {
	for name, tc := range map[string]struct {
		resolved bool
		link     string // target of /etc/resolv.conf; "" writes a regular file
		want     Status
		detail   string
	}{
		"resolved inactive, plain file":       {want: OK},
		"resolved on the runtime stub":        {resolved: true, link: "/run/systemd/resolve/stub-resolv.conf", want: OK},
		"resolved on a relative stub link":    {resolved: true, link: "../run/systemd/resolve/stub-resolv.conf", want: OK},
		"resolved on the static stub":         {resolved: true, link: "/usr/lib/systemd/resolv.conf", want: OK},
		"resolved behind a plain file":        {resolved: true, want: Warning, detail: "is not a link"},
		"resolved behind its uplink file":     {resolved: true, link: "/run/systemd/resolve/resolv.conf", want: Warning, detail: "links to /run/systemd/resolve/resolv.conf"},
		"resolved behind NetworkManager file": {resolved: true, link: "/run/NetworkManager/resolv.conf", want: Warning, detail: "links to"},
	} {
		t.Run(name, func(t *testing.T) {
			orig := unitActive
			t.Cleanup(func() { unitActive = orig })
			unitActive = func(unit string) bool { return tc.resolved && unit == "systemd-resolved.service" }
			root := t.TempDir()
			path := filepath.Join(root, resolvConf)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			var err error
			if tc.link == "" {
				err = os.WriteFile(path, []byte("nameserver 192.0.2.1\n"), 0o644)
			} else {
				err = os.Symlink(tc.link, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			r := findResult(t, CheckHostAt(root), resolvConf)
			if r.Status != tc.want {
				t.Fatalf("status = %s %q, want %s", r.Status, r.Detail, tc.want)
			}
			if !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("detail %q does not contain %q", r.Detail, tc.detail)
			}
			if r.Status == Warning && HasProblem([]Result{r}) {
				t.Error("a Warning fails preflight; it must only be reported")
			}
		})
	}
}
