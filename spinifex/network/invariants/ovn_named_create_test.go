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

// TestS4_OVNNamedCreatesUseEnsure enforces that production callers outside
// the OVN L1 wrapper never use the bare Create primitives for entities whose
// `Name` is not unique-constrained in OVN NB:
//
//	CreateLogicalRouter, CreateLogicalSwitch, CreatePortGroup
//
// OVN NB allows duplicate Name rows; without the wait-op-protected Ensure
// variants these creates race across nodes and across in-process subscriber
// goroutines, producing duplicate logical entities per intent (see bead
// mulga-siv-146 / failing CI run
// https://github.com/mulgadc/spinifex/actions/runs/26447541192).
//
// ADR-0006 S4.7 — "OVN logical entities owned by spinifex MUST be unique on
// (table, Name). The libovsdb client enforces this via wait-then-insert
// transactions (EnsureLogicalRouter / EnsureLogicalSwitch / EnsurePortGroup).
// Callers outside network/ovn MUST use the Ensure* primitive."
//
// Exempt directories:
//
//	spinifex/network/ovn/         — defines the primitives themselves
//	spinifex/network/invariants/  — this test package
//
// `_test.go` files are exempt — fixtures legitimately build entities to
// exercise other behaviour without protecting against cross-node races.
func TestS4_OVNNamedCreatesUseEnsure(t *testing.T) {
	const clause = `ADR-0006 S4.7: "OVN logical entities owned by spinifex ` +
		`MUST be unique on (table, Name). The libovsdb client enforces this ` +
		`via wait-then-insert transactions (EnsureLogicalRouter / ` +
		`EnsureLogicalSwitch / EnsurePortGroup). Callers outside network/ovn ` +
		`MUST use the Ensure* primitive."`

	forbidden := map[string]string{
		"CreateLogicalRouter": "EnsureLogicalRouter",
		"CreateLogicalSwitch": "EnsureLogicalSwitch",
		"CreatePortGroup":     "EnsurePortGroup",
	}

	root := filepath.Join(repoRoot(t), "spinifex")
	exemptDirs := map[string]struct{}{
		filepath.Join(root, "network", "ovn"):        {},
		filepath.Join(root, "network", "invariants"): {},
	}

	type hit struct {
		file    string
		line    int
		sym     string
		replace string
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
			for exempt := range exemptDirs {
				if path == exempt || strings.HasPrefix(path, exempt+string(filepath.Separator)) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
			replace, bad := forbidden[tail]
			if !bad {
				return true
			}
			pos := fset.Position(call.Pos())
			hits = append(hits, hit{pos.Filename, pos.Line, tail, replace})
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
	limit := 8
	for i, h := range hits {
		if i >= limit {
			b.WriteString("  …\n")
			break
		}
		b.WriteString("  ")
		b.WriteString(relTo(h.file, rootForRel))
		b.WriteString(":")
		b.WriteString(itoa(h.line))
		b.WriteString(": calls ")
		b.WriteString(h.sym)
		b.WriteString(" — use ")
		b.WriteString(h.replace)
		b.WriteString(" instead\n")
	}
	b.WriteString("  Fix: swap the call site to the Ensure* primitive ")
	b.WriteString("(returns canonical row; survives cross-node creation race).\n")
	if len(hits) > limit {
		b.WriteString("  ")
		b.WriteString(itoa(len(hits) - limit))
		b.WriteString(" further violations suppressed.\n")
	}
	t.Fatalf("%s", b.String())
}
