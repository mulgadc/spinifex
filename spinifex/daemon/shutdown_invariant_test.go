package daemon_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// awaitShutdown blocks on shutdownWg, and every goroutine registered with
// shutdownWg.Go is a counted member of it. Calling it from inside such a
// goroutine deadlocks: the member cannot decrement until it returns, and it
// cannot return until the counter it is waiting on reaches zero.
//
// startCluster runs as a shutdownWg member and once called awaitShutdown at its
// tail, so the daemon never exited on SIGTERM and was SIGKILLed at
// TimeoutStopSec on every stop. Pinning both helpers to a single call site each
// keeps that shape from returning — a second call site is the defect.
func TestShutdownHelpersHaveASingleCallSite(t *testing.T) {
	for _, tc := range []struct {
		helper string
		caller string
		why    string
	}{
		{
			helper: "awaitShutdown",
			caller: "Start",
			why:    "blocks on shutdownWg; a second caller is almost certainly a member of that group and will deadlock",
		},
		{
			helper: "setupShutdown",
			caller: "Start",
			why:    "each call registers its own signal.Notify and shutdownWg goroutine, so a second caller duplicates the handler",
		},
	} {
		t.Run(tc.helper, func(t *testing.T) {
			sites := callSites(t, tc.helper)
			if len(sites) == 1 && sites[0].caller == tc.caller {
				return
			}

			var b strings.Builder
			fmt.Fprintf(&b, "%s must be called exactly once, from %s — %s\n",
				tc.helper, tc.caller, tc.why)
			for _, s := range sites {
				fmt.Fprintf(&b, "  %s:%d: called from %s\n", s.file, s.line, s.caller)
			}
			t.Fatal(b.String())
		})
	}
}

type callSite struct {
	file   string
	line   int
	caller string
}

// callSites reports every call to a method on the daemon receiver, with the
// enclosing function of each.
func callSites(t *testing.T, method string) []callSite {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var found []callSite
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}

		// Track the enclosing FuncDecl so a hit can name its caller rather than
		// just its line.
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name == nil || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				sel, ok := inner.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != method {
					return true
				}
				pos := fset.Position(sel.Pos())
				found = append(found, callSite{filepath.Base(pos.Filename), pos.Line, fn.Name.Name})
				return true
			})
			return true
		})
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].file != found[j].file {
			return found[i].file < found[j].file
		}
		return found[i].line < found[j].line
	})
	return found
}
