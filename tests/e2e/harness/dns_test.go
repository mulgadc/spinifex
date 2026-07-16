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

func TestNorthstarInternalDomain(t *testing.T) {
	dir := t.TempDir()
	config := `node = "dev1"
[nodes.dev1.northstar]
internal_domain = "compute.internal"
`
	if err := os.WriteFile(filepath.Join(dir, "spinifex.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	if got := NorthstarInternalDomain(&Env{ConfigDir: dir}); got != "compute.internal" {
		t.Fatalf("NorthstarInternalDomain = %q, want %q", got, "compute.internal")
	}
}

func TestRequireDNSEnabled(t *testing.T) {
	dir := t.TempDir()
	config := `[nodes.dev1.northstar]
default_domain = "spx3.net"
`
	if err := os.WriteFile(filepath.Join(dir, "spinifex.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	if got := RequireDNSEnabled(t, &Env{ConfigDir: dir}); got != "spx3.net" {
		t.Fatalf("RequireDNSEnabled = %q, want %q", got, "spx3.net")
	}
}

func TestNorthstarDomainsMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  *Env
	}{
		{name: "nil environment"},
		{name: "no config directory", env: &Env{}},
		{name: "missing config file", env: &Env{ConfigDir: t.TempDir()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NorthstarBaseDomain(tc.env); got != "" {
				t.Fatalf("NorthstarBaseDomain = %q, want empty", got)
			}
			if got := NorthstarInternalDomain(tc.env); got != "" {
				t.Fatalf("NorthstarInternalDomain = %q, want empty", got)
			}
		})
	}
}
