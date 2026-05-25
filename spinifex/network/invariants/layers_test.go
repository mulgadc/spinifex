package invariants

import (
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// TestS1_LayerSkipProhibited enforces ADR-0006 clause S1.
//
//	"No code above layer Lk calls through to a layer below Lk's immediate
//	 interface. Every cross-layer call passes through the typed interface
//	 of the immediate lower neighbor."
//
// Mechanic: derive each network/* package's layer from layerOf(). Reject
// any import where the importer's layer is strictly lower than the
// importee's layer (i.e. an upward edge in the layer graph). Cross-cutters
// (reconcile, subscribers) may import any layer, but no layer may import
// them — that direction would re-introduce the tangling the redesign
// removed.
func TestS1_LayerSkipProhibited(t *testing.T) {
	const clause = `ADR-0006 S1: "No code above layer Lk calls through to a ` +
		`layer below Lk's immediate interface. Every cross-layer call ` +
		`passes through the typed interface of the immediate lower neighbor."`

	pkgs := listNetworkPackages(t)

	type violation struct {
		from     string
		fromKind packageKind
		to       string
		toKind   packageKind
		reason   string
	}
	var bad []violation

	for _, p := range pkgs {
		fromKind := layerOf(p.ImportPath)
		for _, imp := range p.Imports {
			if !strings.HasPrefix(imp, networkRoot) {
				continue
			}
			toKind := layerOf(imp)
			reason := classify(fromKind, toKind)
			if reason == "" {
				continue
			}
			bad = append(bad, violation{
				from: p.ImportPath, fromKind: fromKind,
				to: imp, toKind: toKind,
				reason: reason,
			})
		}
	}

	if len(bad) == 0 {
		return
	}

	sort.Slice(bad, func(i, j int) bool {
		if bad[i].from != bad[j].from {
			return bad[i].from < bad[j].from
		}
		return bad[i].to < bad[j].to
	})

	var b strings.Builder
	b.WriteString(clause)
	b.WriteString("\n")
	limit := 5
	for i, v := range bad {
		if i >= limit {
			b.WriteString("  …\n")
			break
		}
		b.WriteString("  ")
		b.WriteString(shortPkg(v.from))
		b.WriteString(" (")
		b.WriteString(v.fromKind.String())
		b.WriteString(") -> ")
		b.WriteString(shortPkg(v.to))
		b.WriteString(" (")
		b.WriteString(v.toKind.String())
		b.WriteString("): ")
		b.WriteString(v.reason)
		b.WriteString("\n")
	}
	b.WriteString("  Fix: route the call through the immediate lower ")
	b.WriteString("layer's typed interface, or move the offending code to ")
	b.WriteString("an orchestrator (reconcile / subscribers).\n")
	if len(bad) > limit {
		b.WriteString("  ")
		b.WriteString(itoa(len(bad) - limit))
		b.WriteString(" further violations suppressed.\n")
	}
	t.Fatalf("%s", b.String())
}

// packageKind tags a package by its position in the ADR-0006 layer model.
type packageKind int

const (
	kindUnknown packageKind = iota
	kindL0Host
	kindL1OVN
	kindL2Topology
	kindL3Policy
	kindL4Federation
	kindL5External
	kindCrossCutter // reconcile, subscribers — orchestrators, not layers
	kindInvariants  // this package
)

func (k packageKind) String() string {
	switch k {
	case kindL0Host:
		return "L0/host"
	case kindL1OVN:
		return "L1/ovn"
	case kindL2Topology:
		return "L2/topology"
	case kindL3Policy:
		return "L3/policy"
	case kindL4Federation:
		return "L4/federation"
	case kindL5External:
		return "L5/external"
	case kindCrossCutter:
		return "cross-cutter"
	case kindInvariants:
		return "invariants"
	default:
		return "unknown"
	}
}

// rank returns the ADR layer number for ordered layers. Non-layered
// packages return -1 and are handled separately in classify().
func (k packageKind) rank() int {
	switch k {
	case kindL0Host:
		return 0
	case kindL1OVN:
		return 1
	case kindL2Topology:
		return 2
	case kindL3Policy:
		return 3
	case kindL4Federation:
		return 4
	case kindL5External:
		return 5
	}
	return -1
}

const networkRoot = "github.com/mulgadc/spinifex/spinifex/network"

func layerOf(importPath string) packageKind {
	suffix := strings.TrimPrefix(importPath, networkRoot)
	suffix = strings.TrimPrefix(suffix, "/")
	switch {
	case suffix == "host" || strings.HasPrefix(suffix, "host/"):
		return kindL0Host
	case suffix == "ovn" || strings.HasPrefix(suffix, "ovn/"):
		return kindL1OVN
	case suffix == "topology" || strings.HasPrefix(suffix, "topology/"):
		return kindL2Topology
	case suffix == "policy" || strings.HasPrefix(suffix, "policy/"):
		return kindL3Policy
	case suffix == "federation" || strings.HasPrefix(suffix, "federation/"):
		return kindL4Federation
	case suffix == "external" || strings.HasPrefix(suffix, "external/"):
		return kindL5External
	case suffix == "reconcile" || strings.HasPrefix(suffix, "reconcile/"):
		return kindCrossCutter
	case suffix == "subscribers" || strings.HasPrefix(suffix, "subscribers/"):
		return kindCrossCutter
	case suffix == "invariants" || strings.HasPrefix(suffix, "invariants/"):
		return kindInvariants
	}
	return kindUnknown
}

// classify returns a non-empty reason iff the edge from→to violates S1.
// Empty string means the edge is permitted.
func classify(from, to packageKind) string {
	if from == to {
		return ""
	}
	if from == kindInvariants || to == kindInvariants {
		return ""
	}
	// No layer may import a cross-cutter — that would re-tangle the tree.
	if to == kindCrossCutter && from != kindCrossCutter {
		return "layer importing a cross-cutter (reconcile/subscribers)"
	}
	// Cross-cutters may import any layer.
	if from == kindCrossCutter {
		return ""
	}
	fr, tr := from.rank(), to.rank()
	if fr < 0 || tr < 0 {
		return ""
	}
	if tr > fr {
		return "upward layer import (L" + itoa(fr) + " -> L" + itoa(tr) + ")"
	}
	return ""
}

// goListPackage mirrors the subset of `go list -json` fields the test needs.
type goListPackage struct {
	ImportPath string   `json:"ImportPath"`
	Imports    []string `json:"Imports"`
}

func listNetworkPackages(t *testing.T) []goListPackage {
	t.Helper()
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var pkgs []goListPackage
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p goListPackage
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		if !strings.HasPrefix(p.ImportPath, networkRoot) {
			continue
		}
		// Skip the invariants package itself.
		if layerOf(p.ImportPath) == kindInvariants {
			continue
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		t.Fatalf("go list returned no packages under %s", networkRoot)
	}
	return pkgs
}

// repoRoot walks up from the test binary's working directory to find the
// spinifex go.mod, so `go list` runs in the right module regardless of
// where the test runner is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == "/dev/null" {
		t.Fatalf("not in a go module (GOMOD=%q)", gomod)
	}
	return strings.TrimSuffix(gomod, "/go.mod")
}

func shortPkg(p string) string {
	return strings.TrimPrefix(p, networkRoot+"/")
}

// itoa avoids pulling strconv into the failure-format hot path; pure
// stdlib readability win, no allocation concern.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
