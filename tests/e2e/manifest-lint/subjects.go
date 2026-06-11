package manifestlint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	manifestcheck "github.com/mulgadc/spinifex/tests/e2e/manifest-check"
)

// natsCallMethods are NATS connection methods whose first string-literal
// argument is a subject. Dynamic args (fmt.Sprintf, vars, concatenation) are
// not statically resolvable and are skipped.
var natsCallMethods = map[string]bool{
	"Subscribe": true, "QueueSubscribe": true, "SubscribeSync": true,
	"ChanSubscribe": true, "Publish": true, "PublishRequest": true, "Request": true,
}

// subjectLitRE matches a clean, fully-static NATS subject literal. Literals
// containing format verbs/spaces (log lines, fmt.Sprintf templates) fail this
// and are treated as dynamic.
var subjectLitRE = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[A-Za-z0-9_-]+)+$`)

type codeSubject struct {
	subject string
	file    string
	line    int
}

// SubjectLint cross-references NATS subject literals in service source against
// the manifest. repoRoot is the spinifex repo root; m is the loaded manifest.
func SubjectLint(repoRoot string, m *manifestcheck.Manifest) (*Result, error) {
	res := &Result{}

	exact, wildcards, managed := manifestSubjects(m)

	dirs := serviceDirs(m)
	var found []codeSubject
	seenExact := map[string]bool{}
	for _, d := range dirs {
		subs, err := scanSubjectsInDir(filepath.Join(repoRoot, filepath.FromSlash(d)), repoRoot)
		if err != nil {
			return nil, err
		}
		for _, cs := range subs {
			seenExact[cs.subject] = true
			found = append(found, cs)
		}
	}

	// Code subjects not declared in the manifest (ratcheted failures).
	for _, cs := range found {
		if !managed[firstSegment(cs.subject)] {
			continue // out of scope (test.*, system.* internal topics, ...)
		}
		if subjectCovered(cs.subject, exact, wildcards) {
			continue
		}
		key := tab("subject", cs.file, cs.subject)
		msg := fmt.Sprintf("%s:%d: NATS subject %q not declared in service-interfaces.yaml",
			cs.file, cs.line, cs.subject)
		res.add(key, msg)
	}

	// Orphan manifest subjects: declared, managed-prefix, non-wildcard, with no
	// static reference. Advisory only — the real reference may be dynamic.
	var orphans []string
	for s := range exact {
		if !managed[firstSegment(s)] || strings.HasSuffix(s, ".*") {
			continue
		}
		if !seenExact[s] {
			orphans = append(orphans, s)
		}
	}
	sort.Strings(orphans)
	for _, s := range orphans {
		res.warn(fmt.Sprintf("manifest subject %q has no static reference in source (dynamic, or stale entry)", s))
	}

	return res, nil
}

// manifestSubjects returns the exact (non-wildcard) subject set, the wildcard
// prefixes (".*" stripped), and the managed first-segment set.
func manifestSubjects(m *manifestcheck.Manifest) (exact map[string]bool, wildcards []string, managed map[string]bool) {
	exact = map[string]bool{}
	managed = map[string]bool{}
	wset := map[string]bool{}
	for _, svc := range m.Services {
		for _, lists := range [][]string{svc.Subscribes, svc.Publishes, svc.DependsOn} {
			for _, s := range lists {
				managed[firstSegment(s)] = true
				if strings.HasSuffix(s, ".*") {
					wset[strings.TrimSuffix(s, "*")] = true // "ec2.*" -> "ec2."
				} else {
					exact[s] = true
				}
			}
		}
	}
	for w := range wset {
		wildcards = append(wildcards, w)
	}
	sort.Strings(wildcards)
	return exact, wildcards, managed
}

func subjectCovered(s string, exact map[string]bool, wildcards []string) bool {
	if exact[s] {
		return true
	}
	for _, w := range wildcards {
		if strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

// serviceDirs returns the deduplicated, slash-form source roots of every
// service (path + additional_paths).
func serviceDirs(m *manifestcheck.Manifest) []string {
	set := map[string]bool{}
	for _, svc := range m.Services {
		if svc.Path != "" {
			set[svc.Path] = true
		}
		for _, p := range svc.AdditionalPaths {
			set[p] = true
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// scanSubjectsInDir walks dir recursively, parsing every non-test .go file and
// extracting subject literals from natsSub{...} registry entries and from
// literal first-args to NATS connection methods.
func scanSubjectsInDir(dir, repoRoot string) ([]codeSubject, error) {
	var out []codeSubject
	fset := token.NewFileSet()
	err := filepathWalkGo(dir, func(path string) error {
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				// Standalone `natsSub{"subject", ...}` (append form).
				if isIdent(node.Type, "natsSub") {
					addFirstElt(&out, node, rel, fset)
					return true
				}
				// `[]natsSub{ {"subject", ...}, ... }` — inner elements have an
				// elided type, so match on the array element type instead.
				if at, ok := node.Type.(*ast.ArrayType); ok && isIdent(at.Elt, "natsSub") {
					for _, el := range node.Elts {
						if cl, ok := el.(*ast.CompositeLit); ok {
							addFirstElt(&out, cl, rel, fset)
						}
					}
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || !natsCallMethods[sel.Sel.Name] || len(node.Args) == 0 {
					return true
				}
				if s, ok := stringLit(node.Args[0]); ok {
					addSubject(&out, s, rel, fset.Position(node.Pos()).Line)
				}
			}
			return true
		})
		return nil
	})
	return out, err
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func addFirstElt(out *[]codeSubject, cl *ast.CompositeLit, file string, fset *token.FileSet) {
	if len(cl.Elts) == 0 {
		return
	}
	if s, ok := stringLit(cl.Elts[0]); ok {
		addSubject(out, s, file, fset.Position(cl.Pos()).Line)
	}
}

func addSubject(out *[]codeSubject, s, file string, line int) {
	if subjectLitRE.MatchString(s) {
		*out = append(*out, codeSubject{subject: s, file: file, line: line})
	}
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func firstSegment(s string) string {
	if seg, _, ok := strings.Cut(s, "."); ok {
		return seg
	}
	return s
}

// filepathWalkGo invokes fn for every non-test .go file under root, skipping
// testdata and a missing root (a manifest path may legitimately not exist on a
// given checkout).
func filepathWalkGo(root string, fn func(path string) error) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		return fn(path)
	})
}
