package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestS4_OVNNamesL2Owned enforces ADR-0006 clause S4.
//
//	"All OVN NB DB object names are constructed exclusively by L2 using
//	 the naming contract table. No layer above or below L2 constructs OVN
//	 object names independently."
func TestS4_OVNNamesL2Owned(t *testing.T) {
	const clause = `ADR-0006 S4: "All OVN NB DB object names are constructed ` +
		`exclusively by L2 using the naming contract table. No layer above ` +
		`or below L2 constructs OVN object names independently."`

	roots := s4ScanRoots(t)
	type hit struct {
		file string
		line int
		col  int
		lit  string
		via  string // "concat", "sprintf", "join"
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
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if isS4Exempt(path) {
				return nil
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BinaryExpr:
					if x.Op != token.ADD {
						return true
					}
					lit, ok := stringLit(x.X)
					if !ok {
						return true
					}
					if !hasOVNPrefix(lit) {
						return true
					}
					pos := fset.Position(x.Pos())
					hits = append(hits, hit{pos.Filename, pos.Line, pos.Column, lit, "concat"})
				case *ast.CallExpr:
					name := calleeName(x.Fun)
					switch name {
					case "fmt.Sprintf":
						if len(x.Args) < 1 {
							return true
						}
						lit, ok := stringLit(x.Args[0])
						if !ok {
							return true
						}
						if !hasOVNPrefix(lit) {
							return true
						}
						pos := fset.Position(x.Pos())
						hits = append(hits, hit{pos.Filename, pos.Line, pos.Column, lit, "sprintf"})
					case "strings.Join":
						if len(x.Args) < 1 {
							return true
						}
						comp, ok := x.Args[0].(*ast.CompositeLit)
						if !ok {
							return true
						}
						for _, e := range comp.Elts {
							lit, ok := stringLit(e)
							if !ok {
								continue
							}
							if !hasOVNPrefix(lit) {
								continue
							}
							pos := fset.Position(e.Pos())
							hits = append(hits, hit{pos.Filename, pos.Line, pos.Column, lit, "join"})
						}
					}
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
		b.WriteString(":")
		b.WriteString(itoa(h.col))
		b.WriteString(": ")
		b.WriteString(h.via)
		b.WriteString(" of ")
		b.WriteString(strconv.Quote(h.lit))
		b.WriteString("\n")
	}
	b.WriteString("  Fix: add a helper to spinifex/network/topology/names.go ")
	b.WriteString("and call it from the offending site. Do not construct OVN ")
	b.WriteString("object names outside L2.\n")
	if len(hits) > limit {
		b.WriteString("  ")
		b.WriteString(itoa(len(hits) - limit))
		b.WriteString(" further violations suppressed.\n")
	}
	t.Fatalf("%s", b.String())
}

// ovnNamePrefixes is the ADR-0006 contract table; keep in lockstep with topology/names.go.
var ovnNamePrefixes = []string{
	"vpc-",
	"subnet-",
	"port-",
	"rtr-",
	"rtr-port-",
	"gw-",
	"gw-port-",
	"ext-",
	"ext-port-",
	"ts-",
	"trp-",
}

// awsIDPrefixes are AWS resource-IDs that share leading bytes with ovnNamePrefixes; excluded from hasOVNPrefix.
var awsIDPrefixes = []string{
	"vpc-cidr-assoc-",
}

func hasOVNPrefix(s string) bool {
	for _, a := range awsIDPrefixes {
		if strings.HasPrefix(s, a) {
			return false
		}
	}
	for _, p := range ovnNamePrefixes {
		if len(s) > len(p) && strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			return id.Name + "." + f.Sel.Name
		}
	case *ast.Ident:
		return f.Name
	}
	return ""
}

func isS4Exempt(path string) bool {
	clean := filepath.ToSlash(path)
	switch {
	case strings.HasSuffix(clean, "/network/topology/names.go"):
		return true
	case strings.HasSuffix(clean, "/network/topology/names_test.go"):
		return true
	case strings.Contains(clean, "/network/invariants/"):
		return true
	}
	return false
}

func isVendoredOrGenerated(dir string) bool {
	switch dir {
	case "vendor", "testdata", "node_modules", ".git":
		return true
	}
	return false
}

func s4ScanRoots(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	return []string{
		filepath.Join(root, "spinifex", "network"),
		filepath.Join(root, "spinifex", "vpcd"),
		filepath.Join(root, "spinifex", "daemon"),
		filepath.Join(root, "spinifex", "handlers"),
		filepath.Join(root, "spinifex", "vm"),
	}
}

func relTo(p, root string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}
