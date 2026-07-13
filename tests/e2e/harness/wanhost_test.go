//go:build e2e

package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAdvertiseIP(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "advertise preferred over host and ignores sub-tables",
			toml: `node = "wattle"
[nodes.wattle]
host = "10.105.1.3"
advertise = "192.168.1.13"
[nodes.wattle.awsgw]
host = "10.105.1.3:9999"
`,
			want: "192.168.1.13",
		},
		{
			name: "falls back to host when advertise unset",
			toml: `node = "dev1"
[nodes.dev1]
host = "192.168.1.20"
`,
			want: "192.168.1.20",
		},
		{
			name: "resolves only the local node, not co-tenant nodes",
			toml: `node = "n2"
[nodes.n1]
host = "10.0.0.1"
advertise = "1.1.1.1"
[nodes.n2]
host = "10.0.0.2"
advertise = "2.2.2.2"
`,
			want: "2.2.2.2",
		},
		{
			name: "empty when top-level node key is absent",
			toml: `[nodes.x]
advertise = "9.9.9.9"
`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "spinifex.toml"), []byte(tc.toml), 0o600); err != nil {
				t.Fatalf("write toml: %v", err)
			}
			if got := advertiseIP(dir); got != tc.want {
				t.Fatalf("advertiseIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAdvertiseIPMissing(t *testing.T) {
	if got := advertiseIP(""); got != "" {
		t.Fatalf("advertiseIP(\"\") = %q, want empty", got)
	}
	if got := advertiseIP(t.TempDir()); got != "" {
		t.Fatalf("advertiseIP(missing file) = %q, want empty", got)
	}
}
