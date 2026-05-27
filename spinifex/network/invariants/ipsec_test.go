package invariants

import (
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// TestS8_IPSecOVNNativeOnly enforces ADR-0006 clause S8.
//
//	"IPSec is OVN-native only. No layer implements custom IKEv2, XFRM
//	 rules, or direct strongSwan management. IPSec SA lifecycle is
//	 delegated entirely to OVN native IPSec and is invisible above L0."
func TestS8_IPSecOVNNativeOnly(t *testing.T) {
	const clause = `ADR-0006 S8: "IPSec is OVN-native only. No layer ` +
		`implements custom IKEv2, XFRM rules, or direct strongSwan ` +
		`management. IPSec SA lifecycle is delegated entirely to OVN ` +
		`native IPSec and is invisible above L0."`

	deniedSubstrings := []string{
		"strongswan",
		"strongSwan",
		"/xfrm",
		"netlink/nl/xfrm",
		"ikev2",
		"IKEv2",
		"libreswan",
		"openswan",
	}

	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	type hit struct {
		pkg string
		imp string
	}
	var hits []hit
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p goListPackage
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		if !strings.HasPrefix(p.ImportPath, networkRoot) {
			continue
		}
		for _, imp := range p.Imports {
			for _, sub := range deniedSubstrings {
				if strings.Contains(imp, sub) {
					hits = append(hits, hit{p.ImportPath, imp})
				}
			}
		}
	}

	if len(hits) == 0 {
		return
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].pkg != hits[j].pkg {
			return hits[i].pkg < hits[j].pkg
		}
		return hits[i].imp < hits[j].imp
	})

	var b strings.Builder
	b.WriteString(clause)
	b.WriteString("\n")
	for _, h := range hits {
		b.WriteString("  ")
		b.WriteString(shortPkg(h.pkg))
		b.WriteString(" imports ")
		b.WriteString(h.imp)
		b.WriteString("\n")
	}
	b.WriteString("  Fix: remove the import. IPSec SA lifecycle is handled ")
	b.WriteString("by OVN native IPSec (strongSwan-as-OVN-internal); do not ")
	b.WriteString("manage IKEv2 / XFRM / strongSwan directly from Go.\n")
	t.Fatalf("%s", b.String())
}
