package layering

import (
	"os"
	"path/filepath"
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
	"vpcd",
}

// retiredLegacyRoots were legacy roots whose every package has moved to its
// ADR-0001 target; recreating one would reopen a closed migration row.
var retiredLegacyRoots = []string{"services", "utils"}

// TestADR0001_LegacyRootsFrozen: the current-to-target map gives every legacy
// root a destination, so none may gain a package and no retired root may return.
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

	for _, r := range retiredLegacyRoots {
		entries, err := os.ReadDir(filepath.Join(root, "spinifex", r))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(entries) > 0 {
			t.Errorf("ADR-0001 current-to-target map: spinifex/%s is a retired legacy root; give the code its target home instead", r)
		}
	}
}
