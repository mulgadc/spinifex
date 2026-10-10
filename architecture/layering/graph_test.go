// Package layering enforces ADR-0001's package-boundary dependency rules over
// the module's production import graph, ratcheted by a recorded-debt allowlist.
package layering

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const modulePath = "github.com/mulgadc/spinifex"

// edge is one package-to-package import, both sides named as in the allowlist:
// application packages relative to spinifex/, module-root ones (cmd/,
// contracts/, internal/) relative to the module root.
type edge struct {
	From, To string
}

func (e edge) String() string { return e.From + " -> " + e.To }

// graph is the production import graph restricted to this module.
type graph struct {
	// pkgs maps a package name to its module-relative directory.
	pkgs map[string]string
	// imports records the first file:line at which each edge appears.
	imports map[edge]string
}

func newGraph() *graph {
	return &graph{pkgs: map[string]string{}, imports: map[edge]string{}}
}

// shortName drops the application-root prefix so names read like the ADR and
// the migration record; classify rejects app-root dirs that would collide.
func shortName(full string) string {
	if rest, ok := strings.CutPrefix(full, "spinifex/"); ok {
		return rest
	}
	return full
}

func moduleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot locate test source")
	}
	// architecture/layering/graph_test.go -> module root.
	return filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
}

// excludedTopDirs hold no production code: tests/ is test-only, docs/ only
// embeds documentation and architecture/ is repository governance tooling.
var excludedTopDirs = map[string]bool{"tests": true, "docs": true, "architecture": true}

// loadGraph parses the imports of every non-test Go file under root, so it
// sees every build-tag variant and needs no Go toolchain or workspace.
func loadGraph(root string) (*graph, error) {
	g := newGraph()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "vendor" || name == "node_modules" || excludedTopDirs[rel] {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return err
		}
		if ignoredByBuildTag(f.Comments, f.Package) {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}
		from := shortName(dir)
		g.pkgs[from] = dir
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			target, ok := strings.CutPrefix(p, modulePath+"/")
			if !ok {
				continue
			}
			e := edge{From: from, To: shortName(target)}
			if _, seen := g.imports[e]; !seen {
				pos := fset.Position(imp.Pos())
				g.imports[e] = fmt.Sprintf("%s:%d", rel, pos.Line)
			}
		}
		return nil
	})
	return g, err
}

// ignoredByBuildTag reports a `//go:build ignore` file, which is a standalone
// program (a generator) rather than part of its directory's package.
func ignoredByBuildTag(groups []*ast.CommentGroup, pkgPos token.Pos) bool {
	for _, cg := range groups {
		if cg.Pos() >= pkgPos {
			break
		}
		for _, c := range cg.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err == nil && expr.String() == "ignore" {
				return true
			}
		}
	}
	return false
}

// sortedEdges returns the graph's edges in a stable order.
func (g *graph) sortedEdges() []edge {
	out := make([]edge, 0, len(g.imports))
	for e := range g.imports {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}
