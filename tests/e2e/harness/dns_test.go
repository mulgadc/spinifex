//go:build e2e

package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNorthstarBaseDomain(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			// The shape `spx admin init` writes: the domain rides the local node's
			// northstar sub-table alongside config_path.
			name: "resolves the local node's northstar default_domain",
			toml: `node = "dev1"
[nodes.dev1]
host = "192.168.1.20"
[nodes.dev1.northstar]
config_path = "/etc/spinifex/northstar/northstar.toml"
default_domain = "spx3.net"
internal_domain = "compute.internal"
`,
			want: "spx3.net",
		},
		{
			name: "prefers the local node over co-tenant stanzas",
			toml: `node = "n2"
[nodes.n1.northstar]
default_domain = "wrong.example"
[nodes.n2.northstar]
default_domain = "right.example"
`,
			want: "right.example",
		},
		{
			// The zone is cluster-wide, so any node carrying it answers when the
			// local stanza is absent or domainless.
			name: "falls back to a peer node when the local stanza has no domain",
			toml: `node = "n1"
[nodes.n1]
host = "10.0.0.1"
[nodes.n2.northstar]
default_domain = "peer.example"
`,
			want: "peer.example",
		},
		{
			name: "empty when northstar is not configured",
			toml: `node = "dev1"
[nodes.dev1]
host = "192.168.1.20"
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
			if got := NorthstarBaseDomain(&Env{ConfigDir: dir}); got != tc.want {
				t.Fatalf("NorthstarBaseDomain = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNorthstarBaseDomainMissing(t *testing.T) {
	if got := NorthstarBaseDomain(nil); got != "" {
		t.Fatalf("NorthstarBaseDomain(nil) = %q, want empty", got)
	}
	if got := NorthstarBaseDomain(&Env{}); got != "" {
		t.Fatalf("NorthstarBaseDomain(no ConfigDir) = %q, want empty", got)
	}
	if got := NorthstarBaseDomain(&Env{ConfigDir: t.TempDir()}); got != "" {
		t.Fatalf("NorthstarBaseDomain(missing file) = %q, want empty", got)
	}
}
