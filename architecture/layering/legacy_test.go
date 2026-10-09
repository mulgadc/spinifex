package layering

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// frozenLegacyPackages is every production package still under a legacy root.
// It only shrinks: a new entry is a new legacy package, a missing one is a
// completed move whose line must be deleted.
var frozenLegacyPackages = []string{
	"accountteardown",
	"admin",
	"daemon",
	"gateway",
	"gateway/bedrock",
	"gateway/ecs",
	"gateway/eks",
	"gateway/elbv2",
	"gateway/iam",
	"gateway/rds",
	"gateway/spx",
	"gateway/sts",
	"gateway/tagging",
	"handlers/ecs",
	"handlers/ecs/bus",
	"handlers/eks",
	"handlers/elbv2",
	"handlers/iam",
	"handlers/iam/mock",
	"handlers/rds",
	"handlers/sts",
	"lbagent",
	"services/viperblockd",
	"services/viperblockd/vbwire",
	"utils",
	"vpcd",
}

// frozenUtilsFiles are the only production files utils may hold; each awaits
// the owner named in the migration record's close-out table.
var frozenUtilsFiles = []string{"encryption.go", "vpcd_event.go"}

// TestADR0001_LegacyRootsFrozen: the current-to-target map gives every legacy
// root a destination, so none may gain a package and utils no new file.
func TestADR0001_LegacyRootsFrozen(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph(root)
	if err != nil {
		t.Fatal(err)
	}

	present := map[string]bool{}
	for name, dir := range g.pkgs {
		if c, ok := classify(dir); ok && c.layer == layerLegacy {
			present[name] = true
		}
	}
	if len(present) == 0 {
		t.Fatal("found no legacy packages; the freeze would pass vacuously")
	}
	frozen := map[string]bool{}
	for _, p := range frozenLegacyPackages {
		frozen[p] = true
		if !present[p] {
			t.Errorf("legacy package %s no longer exists; delete it from frozenLegacyPackages", p)
		}
	}
	for p := range present {
		if !frozen[p] {
			t.Errorf("ADR-0001 current-to-target map: %s is a new package under a legacy root; create it at its ADR-0001 target home instead", p)
		}
	}

	entries, err := os.ReadDir(filepath.Join(root, "spinifex", "utils"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			t.Errorf("ADR-0001 utils row \"no new utils replacement\": utils/%s is a new directory; give the code a named owner instead", name)
			continue
		}
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	allowedFiles := map[string]bool{}
	for _, f := range frozenUtilsFiles {
		allowedFiles[f] = true
	}
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
		if !allowedFiles[f] {
			t.Errorf("ADR-0001 utils row \"dissolve by responsibility; no new utils replacement\": utils/%s is new; move it to its owner", f)
		}
	}
	for _, f := range frozenUtilsFiles {
		if !have[f] {
			t.Errorf("utils/%s has moved out; delete it from frozenUtilsFiles", f)
		}
	}
}
