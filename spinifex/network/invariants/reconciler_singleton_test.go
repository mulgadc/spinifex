package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestS9_SingleReconcilerNoRetrofitPasses enforces ADR-0006 clause S9.
//
//	"On each leader-gated startup, exactly one intent-actual reconciliation
//	 pass runs against the NATS KV snapshot and OVN NB DB actual state. No
//	 additional serial retrofit passes exist."
func TestS9_SingleReconcilerNoRetrofitPasses(t *testing.T) {
	const clause = `ADR-0006 S9: "On each leader-gated startup, exactly one ` +
		`intent-actual reconciliation pass runs against the NATS KV snapshot ` +
		`and OVN NB DB actual state. No additional serial retrofit passes exist."`

	loopRE := regexp.MustCompile(`^Reconcile[A-Za-z]*Loop$`)
	passRE := regexp.MustCompile(`^Reconcile[A-Za-z]*Pass\d+$`)

	root := filepath.Join(repoRoot(t), "spinifex", "network")
	type hit struct {
		file string
		line int
		sym  string
		why  string
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
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				return true
			}
			name := fn.Name.Name
			switch {
			case loopRE.MatchString(name):
				pos := fset.Position(fn.Pos())
				hits = append(hits, hit{pos.Filename, pos.Line, name, "Reconcile…Loop (retrofit pass)"})
			case passRE.MatchString(name):
				pos := fset.Position(fn.Pos())
				hits = append(hits, hit{pos.Filename, pos.Line, name, "Reconcile…Pass<n> (serial retrofit)"})
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
	for _, h := range hits {
		b.WriteString("  ")
		b.WriteString(relTo(h.file, rootForRel))
		b.WriteString(":")
		b.WriteString(itoa(h.line))
		b.WriteString(": ")
		b.WriteString(h.sym)
		b.WriteString(" — ")
		b.WriteString(h.why)
		b.WriteString("\n")
	}
	b.WriteString("  Fix: collapse this function into the single intent-actual ")
	b.WriteString("reconciler in network/reconcile/. Periodic re-runs schedule ")
	b.WriteString("the same entrypoint; they do not warrant a new function.\n")
	t.Fatalf("%s", b.String())
}
