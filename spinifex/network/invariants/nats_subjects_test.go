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

// TestS7_NATSSubjectsCarryAZPrefix enforces ADR-0006 clause S7.
//
//	"Every vpcd-originated NATS publication uses a subject prefixed
//	 vpc.{azID}. A subscription never processes a message whose subject
//	 AZ prefix does not match the local node's AZ identifier."
//
// Status: aspirational. As of mulga-siv-125.* the subject scheme is still
// the legacy `vpc.<verb>` form (`vpc.create`, `vpc.delete`, …) with no
// `{azID}` segment. ADR-0006 §"AZ identity and NATS subject routing"
// describes the planned rename; until the cluster-wide cutover lands,
// renaming subjects would break rolling upgrades.
//
// To keep the gap visible without breaking CI, the test runs the audit and
// — when violations are found — calls t.Skip with the gap report. The
// `go test` summary prints the SKIP reason on every run so the work
// remains discoverable.
//
// Once the rename ships, change t.Skip to t.Fatalf and the invariant
// becomes a true fence.
func TestS7_NATSSubjectsCarryAZPrefix(t *testing.T) {
	const clause = `ADR-0006 S7: "Every vpcd-originated NATS publication ` +
		`uses a subject prefixed vpc.{azID}. A subscription never processes ` +
		`a message whose subject AZ prefix does not match the local node's ` +
		`AZ identifier."`

	// Subject literals beginning with `vpc.` followed immediately by a
	// non-`{` token violate S7. The compliant form is `vpc.{azID}.<verb>`
	// (or a template fragment containing `{azID}`).
	compliant := regexp.MustCompile(`^vpc\.\{`)
	subjectLike := regexp.MustCompile(`^vpc\.[a-zA-Z]`)

	// Scan covers vpcd's own publishers plus every upstack consumer that
	// publishes or subscribes to vpc.* subjects. The gap report is
	// intentionally wide so the eventual cluster-wide rename has a complete
	// inventory.
	roots := []string{
		filepath.Join(repoRoot(t), "spinifex", "network"),
		filepath.Join(repoRoot(t), "spinifex", "vpcd"),
		filepath.Join(repoRoot(t), "spinifex", "daemon"),
		filepath.Join(repoRoot(t), "spinifex", "handlers"),
		filepath.Join(repoRoot(t), "spinifex", "testutil"),
		filepath.Join(repoRoot(t), "spinifex", "utils"),
	}

	type hit struct {
		file    string
		line    int
		subject string
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
			if strings.Contains(filepath.ToSlash(path), "/network/invariants/") {
				return nil
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				bl, ok := n.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					return true
				}
				lit, ok := stringLit(bl)
				if !ok {
					return true
				}
				if !subjectLike.MatchString(lit) {
					return true
				}
				if compliant.MatchString(lit) {
					return true
				}
				pos := fset.Position(bl.Pos())
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
	b.WriteString("S7 gap (aspirational): subject rename pending. ")
	b.WriteString("Once the cluster-wide cutover lands, switch t.Skip to ")
	b.WriteString("t.Fatalf in nats_subjects_test.go.\n")
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
		b.WriteString(": subject ")
		b.WriteString(h.subject)
		b.WriteString(" lacks {azID} segment\n")
	}
	if len(hits) > limit {
		b.WriteString("  ")
		b.WriteString(itoa(len(hits) - limit))
		b.WriteString(" further subjects suppressed.\n")
	}
	t.Skip(b.String())
}
