package main

//test:in-package — the agent is a main package, which has no external test
// package to import it from, and this covers the unexported default parameter
// set the image bakes.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The include the image build validates against a real mariadbd. It is checked
// in rather than generated during the build because the build runs inside the
// libguestfs appliance, where there is no Go toolchain to resolve the catalog.
const mariadbDefaultParametersFixture = "../../images/mkosi.profiles/rds-mariadb/default-parameters.cnf"

// Byte-for-byte what rds-init installs on a smallest-class instance that
// overrides nothing. Regenerate with RDS_UPDATE_FIXTURES=1 go test ./cmd/rds-agent/.
//
// Keeping it in step matters because setup.sh feeds exactly this file to
// mariadbd: a catalog that grew a name the server refuses at startup would
// otherwise reach a customer as an instance that never boots.
func TestMariaDBDefaultParametersFixture_MatchesTheCatalog(t *testing.T) {
	engine, err := handlers_rds.LookupEngine("mariadb")
	if err != nil {
		t.Fatalf("LookupEngine: %v", err)
	}
	class := handlers_rds.SmallestInstanceClass()
	params, err := engine.ResolveEffectiveParameters(class, nil)
	if err != nil {
		t.Fatalf("ResolveEffectiveParameters(%s): %v", class, err)
	}
	body, err := renderParameters("mariadb", params)
	if err != nil {
		t.Fatalf("renderParameters: %v", err)
	}
	want := mariadbParametersHeader + body

	path := filepath.FromSlash(mariadbDefaultParametersFixture)
	if os.Getenv("RDS_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatalf("update %s: %v", path, err)
		}
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s is stale for class %s.\nRegenerate with RDS_UPDATE_FIXTURES=1 go test ./cmd/rds-agent/\n\ngot:\n%s\nwant:\n%s",
			path, class, got, want)
	}
}

// The one thing the fixture exists to prove cannot regress without the image
// build catching it, asserted here too so a plain preflight names it.
func TestMariaDBDefaultParametersFixture_CarriesNoSetOnlyName(t *testing.T) {
	got, err := os.ReadFile(filepath.FromSlash(mariadbDefaultParametersFixture))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	for _, name := range []string{"\ntime_zone ", "\ntx_isolation ", "\ninnodb_buffer_pool_instances "} {
		if strings.Contains(string(got), name) {
			t.Errorf("the default include carries %q, which mariadbd refuses at startup", name)
		}
	}
}
