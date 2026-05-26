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

// TestS3_NATModeInitTimeConstant enforces ADR-0006 clause S3.
//
//	"NAT distribution mode is an init-time constant. Centralised-vs-
//	 distributed NAT is determined once at startup from L0's UplinkMode()
//	 and never re-evaluated at runtime. No layer above L0 branches on NAT
//	 mode dynamically."
//
// Mechanic: forbid calls to `.UplinkMode()` and `NATModeFromUplinkMode(...)`
// outside the legitimate resolver locations:
//   - network/host/ (defines UplinkMode())
//   - network/policy/policy.go (defines NATModeFromUplinkMode)
//   - vpcd/vpcd.go (single entrypoint resolves once at startup)
//   - any *_test.go (tests legitimately exercise the resolver)
//
// Any additional callsite would be a re-resolution and a S3 violation.
func TestS3_NATModeInitTimeConstant(t *testing.T) {
	const clause = `ADR-0006 S3: "NAT distribution mode is an init-time ` +
		`constant. Centralised-vs-distributed NAT is determined once at ` +
		`startup from L0's UplinkMode() and never re-evaluated at runtime. ` +
		`No layer above L0 branches on NAT mode dynamically."`

	type hit struct {
		file string
		line int
		sym  string
	}
	var hits []hit

	roots := []string{
		filepath.Join(repoRoot(t), "spinifex", "network"),
		filepath.Join(repoRoot(t), "spinifex", "vpcd"),
		filepath.Join(repoRoot(t), "spinifex", "daemon"),
	}
	for _, root := range roots {
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
			if isS3Exempt(path) {
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
				name := calleeName(call.Fun)
				selName := selectorTail(call.Fun)
				switch {
				case selName == "UplinkMode":
					pos := fset.Position(call.Pos())
					hits = append(hits, hit{pos.Filename, pos.Line, selName + "()"})
				case strings.HasSuffix(name, "NATModeFromUplinkMode"):
					pos := fset.Position(call.Pos())
					hits = append(hits, hit{pos.Filename, pos.Line, "NATModeFromUplinkMode"})
				}
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
		b.WriteString(": ")
		b.WriteString(h.sym)
		b.WriteString(" called outside the init-time resolver location\n")
	}
	b.WriteString("  Fix: resolve NAT mode once at startup (vpcd entrypoint) ")
	b.WriteString("and pass the typed enum to constructors. Store as an ")
	b.WriteString("immutable field; do not re-resolve.\n")
	if len(hits) > limit {
		b.WriteString("  ")
		b.WriteString(itoa(len(hits) - limit))
		b.WriteString(" further violations suppressed.\n")
	}
	t.Fatalf("%s", b.String())
}

func isS3Exempt(path string) bool {
	clean := filepath.ToSlash(path)
	switch {
	case strings.HasSuffix(clean, "/network/host"):
		return true
	case strings.Contains(clean, "/network/host/"):
		return true
	case strings.HasSuffix(clean, "/network/policy/policy.go"):
		return true
	case strings.HasSuffix(clean, "/vpcd/vpcd.go"):
		return true
	case strings.Contains(clean, "/network/invariants/"):
		return true
	}
	return false
}

// selectorTail returns the trailing selector name (`.Foo`) of a call
// expression, or "" if the call is not a selector. Used to detect method
// calls regardless of the receiver type.
func selectorTail(fun ast.Expr) string {
	if sel, ok := fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	return ""
}
