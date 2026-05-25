package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestS6_FederationIsolated enforces ADR-0006 clause S6.
//
//	"L4 is the only layer aware of other AZs. No layer below L4 references
//	 remote AZ identifiers, OVN-IC transit switches, or inter-AZ link state.
//	 Cross-AZ federation is always mediated by L4."
//
// Mechanic: scan prod .go files under spinifex/network/ for symbols that
// only the federation layer is allowed to use:
//   - identifiers: TransitSwitch, TransitRouterPort (the L2 name helpers
//     are exempt — L2 owns names; L4 will call into them when it lands)
//   - string literals containing "ovn-ic", "ovn-icnbctl", "ovn-icsbctl"
//
// Exempt locations:
//   - network/topology/names.go (defines the names per ADR table)
//   - network/federation/ (when it exists — currently empty; the fence
//     pre-protects the package)
//   - tests
func TestS6_FederationIsolated(t *testing.T) {
	const clause = `ADR-0006 S6: "L4 is the only layer aware of other AZs. ` +
		`No layer below L4 references remote AZ identifiers, OVN-IC transit ` +
		`switches, or inter-AZ link state. Cross-AZ federation is always ` +
		`mediated by L4."`

	forbiddenIdents := map[string]struct{}{
		"TransitSwitch":     {},
		"TransitRouterPort": {},
	}
	forbiddenLiterals := []string{
		"ovn-ic",
		"ovn-icnbctl",
		"ovn-icsbctl",
	}

	root := filepath.Join(repoRoot(t), "spinifex", "network")
	type hit struct {
		file string
		line int
		sym  string
	}
	var hits []hit

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if isVendoredOrGenerated(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if isS6Exempt(path) {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if _, bad := forbiddenIdents[x.Sel.Name]; bad {
					pos := fset.Position(x.Pos())
					hits = append(hits, hit{pos.Filename, pos.Line, x.Sel.Name})
				}
			case *ast.Ident:
				if _, bad := forbiddenIdents[x.Name]; bad {
					pos := fset.Position(x.Pos())
					hits = append(hits, hit{pos.Filename, pos.Line, x.Name})
				}
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				lit, ok := stringLit(x)
				if !ok {
					return true
				}
				for _, sub := range forbiddenLiterals {
					if strings.Contains(lit, sub) {
						pos := fset.Position(x.Pos())
						hits = append(hits, hit{pos.Filename, pos.Line, "\"" + lit + "\""})
						break
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(hits) == 0 {
		return
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].line < hits[j].line
	})

	rootForRel := repoRoot(t)
	var b strings.Builder
	b.WriteString(clause)
	b.WriteString("\n")
	limit := 5
	for i, h := range hits {
		if i >= limit {
			b.WriteString("  …\n")
			break
		}
		b.WriteString("  ")
		b.WriteString(relTo(h.file, rootForRel))
		b.WriteString(":")
		b.WriteString(itoa(h.line))
		b.WriteString(": federation symbol ")
		b.WriteString(h.sym)
		b.WriteString(" outside L4\n")
	}
	b.WriteString("  Fix: route the inter-AZ operation through network/federation/. ")
	b.WriteString("Only L4 may reference remote-AZ identifiers, OVN-IC objects, ")
	b.WriteString("or inter-AZ link state.\n")
	if len(hits) > limit {
		b.WriteString("  ")
		b.WriteString(itoa(len(hits) - limit))
		b.WriteString(" further violations suppressed.\n")
	}
	t.Fatalf("%s", b.String())
}

func isS6Exempt(path string) bool {
	clean := filepath.ToSlash(path)
	switch {
	case strings.HasSuffix(clean, "/network/topology/names.go"):
		return true
	case strings.Contains(clean, "/network/federation/"):
		return true
	case strings.Contains(clean, "/network/invariants/"):
		return true
	}
	return false
}
