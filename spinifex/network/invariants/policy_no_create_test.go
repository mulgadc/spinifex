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

// TestS5_PolicyNeverCreatesOrDeletes enforces ADR-0006 clause S5.
//
//	"L3 never creates or deletes logical objects. L3 only attaches policy
//	 (ACLs, NAT rules, static routes) to objects that L2 already owns. L3
//	 never calls create or delete on logical switches, routers, or ports."
func TestS5_PolicyNeverCreatesOrDeletes(t *testing.T) {
	const clause = `ADR-0006 S5: "L3 never creates or deletes logical ` +
		`objects. L3 only attaches policy (ACLs, NAT rules, static routes) ` +
		`to objects that L2 already owns. L3 never calls create or delete ` +
		`on logical switches, routers, or ports."`

	forbidden := map[string]struct{}{
		"CreateLogicalSwitch":     {},
		"CreateLogicalRouter":     {},
		"CreateLogicalSwitchPort": {},
		"CreateLogicalRouterPort": {},
		"DeleteLogicalSwitch":     {},
		"DeleteLogicalRouter":     {},
		"DeleteLogicalSwitchPort": {},
		"DeleteLogicalRouterPort": {},
	}

	root := filepath.Join(repoRoot(t), "spinifex", "network", "policy")
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
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			tail := selectorTail(call.Fun)
			if _, bad := forbidden[tail]; !bad {
				return true
			}
			pos := fset.Position(call.Pos())
			hits = append(hits, hit{pos.Filename, pos.Line, tail})
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
		b.WriteString(": L3 calls ")
		b.WriteString(h.sym)
		b.WriteString("\n")
	}
	b.WriteString("  Fix: move the create/delete to L2 (topology) and have ")
	b.WriteString("L3 attach policy only. L3 must never mutate logical ")
	b.WriteString("switch/router/port lifecycle.\n")
	if len(hits) > limit {
		b.WriteString("  ")
		b.WriteString(itoa(len(hits) - limit))
		b.WriteString(" further violations suppressed.\n")
	}
	t.Fatalf("%s", b.String())
}
