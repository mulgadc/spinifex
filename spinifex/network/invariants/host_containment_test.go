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

// TestS2_BridgeModeContainedInL0 enforces ADR-0006 clause S2.
//
//	"Bridge mode is contained in L0. No layer above L0 reads uplink type,
//	 bridge mode, or physical NIC state directly. L0's UplinkMode() is the
//	 only resolver; all layers above receive a typed enum at init time."
func TestS2_BridgeModeContainedInL0(t *testing.T) {
	const clause = `ADR-0006 S2: "Bridge mode is contained in L0. No layer ` +
		`above L0 reads uplink type, bridge mode, or physical NIC state ` +
		`directly. L0's UplinkMode() is the only resolver; all layers ` +
		`above receive a typed enum at init time."`

	hostTools := map[string]struct{}{
		"ovs-vsctl":  {},
		"ovs-ofctl":  {},
		"ovn-nbctl":  {},
		"ovn-sbctl":  {},
		"ovn-appctl": {},
	}

	roots := []string{
		filepath.Join(repoRoot(t), "spinifex", "network"),
		filepath.Join(repoRoot(t), "spinifex", "daemon"),
		filepath.Join(repoRoot(t), "spinifex", "vm"),
	}
	type hit struct {
		file string
		line int
		tool string
	}
	var hits []hit

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if isVendoredOrGenerated(d.Name()) {
					return filepath.SkipDir
				}
				// network/host/ is L0; only layer that may shell out.
				if strings.HasSuffix(filepath.ToSlash(path), "/network/host") {
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
				if len(call.Args) < 1 {
					return true
				}
				lit, ok := stringLit(call.Args[0])
				if !ok {
					return true
				}
				if _, banned := hostTools[lit]; !banned {
					return true
				}
				pos := fset.Position(call.Pos())
				hits = append(hits, hit{pos.Filename, pos.Line, lit})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
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
		b.WriteString(": invokes ")
		b.WriteString(h.tool)
		b.WriteString(" outside network/host/\n")
	}
	b.WriteString("  Fix: move the bridge/NIC operation into network/host/ ")
	b.WriteString("and expose a typed method on host.Wiring. Layers above ")
	b.WriteString("L0 may not shell out to host tools directly.\n")
	if len(hits) > limit {
		b.WriteString("  ")
		b.WriteString(itoa(len(hits) - limit))
		b.WriteString(" further violations suppressed.\n")
	}
	t.Fatalf("%s", b.String())
}
