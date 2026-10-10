package layering

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

type violation struct {
	rule string
	edge edge
	pos  string
}

type report struct {
	unmapped   []string
	violations []violation // every rule hit, allowlisted or not
	unrecorded []violation // hits whose edge has no allowlist entry
	stale      []allowance // entries whose edge no rule rejects any more
	malformed  []string
}

// check classifies g, applies every rule and compares the result with allow.
func check(g *graph, allow []allowance) report {
	var rep report
	classes := map[string]class{}
	for name, dir := range g.pkgs {
		c, ok := classify(dir)
		if !ok || c.layer == layerTests {
			rep.unmapped = append(rep.unmapped, fmt.Sprintf("%s (directory %s)", name, dir))
			continue
		}
		classes[name] = c
	}

	allowed := map[edge]bool{}
	for _, a := range allow {
		e := edge{From: a.From, To: a.To}
		switch {
		case allowed[e]:
			rep.malformed = append(rep.malformed, "duplicate entry "+e.String())
		case a.Debt == "" || a.Reason == "":
			rep.malformed = append(rep.malformed, "entry "+e.String()+" needs both Debt and Reason")
		}
		allowed[e] = true
	}

	hit := map[edge]bool{}
	for _, e := range g.sortedEdges() {
		from, ok := classes[e.From]
		if !ok {
			continue
		}
		to, ok := classify(fullPath(e.To))
		if !ok {
			rep.unmapped = append(rep.unmapped, fmt.Sprintf("%s (imported by %s at %s)", e.To, e.From, g.imports[e]))
			continue
		}
		for _, r := range rules {
			if !r.subject(from, e.From) || !r.forbids(from, to, e.From) {
				continue
			}
			v := violation{rule: r.id, edge: e, pos: g.imports[e]}
			rep.violations = append(rep.violations, v)
			hit[e] = true
			if !allowed[e] {
				rep.unrecorded = append(rep.unrecorded, v)
			}
		}
	}
	for _, a := range allow {
		if !hit[edge{From: a.From, To: a.To}] {
			rep.stale = append(rep.stale, a)
		}
	}
	sort.Strings(rep.unmapped)
	return rep
}

func ruleByID(id string) rule {
	for _, r := range rules {
		if r.id == id {
			return r
		}
	}
	panic("unknown rule " + id)
}

// TestADR0001_Layering is the blocking ratchet over the real import graph.
func TestADR0001_Layering(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	// Guards against a walk that silently finds nothing and passes vacuously.
	if len(g.pkgs) < 100 || len(g.imports) < 500 {
		t.Fatalf("scanned %d packages and %d module imports under %s; the walk has lost the tree and every rule would pass vacuously",
			len(g.pkgs), len(g.imports), root)
	}
	rep := check(g, allowlist)

	t.Run("Classification_EveryPackageMapped", func(t *testing.T) {
		for _, u := range rep.unmapped {
			t.Errorf("ADR-0001 current-to-target map: package %s has no layer; place it under an ADR-0001 home or classify it deliberately in layers_test.go", u)
		}
	})

	for _, r := range rules {
		t.Run(r.id, func(t *testing.T) {
			subjects := 0
			for name, dir := range g.pkgs {
				if c, ok := classify(dir); ok && r.subject(c, name) {
					subjects++
				}
			}
			if subjects == 0 {
				t.Fatalf("no scanned package is governed by %s; the rule would pass vacuously", r.id)
			}
			var snippets []string
			for _, v := range rep.unrecorded {
				if v.rule != r.id {
					continue
				}
				t.Errorf("ADR-0001 %s: %s imports %s (%s); %s",
					r.clause, v.edge.From, v.edge.To, v.pos, r.hint)
				snippets = append(snippets, fmt.Sprintf("{From: %q, To: %q, Debt: \"<migration-record debt subject>\", Reason: \"<why>\"},", v.edge.From, v.edge.To))
			}
			if len(snippets) > 0 {
				t.Logf("fix the import; only debt already recorded in docs/PACKAGE_BOUNDARY_MIGRATION.md may be allow-listed:\n%s",
					strings.Join(snippets, "\n"))
			}
		})
	}

	t.Run("Allowlist_NoStaleEntries", func(t *testing.T) {
		for _, a := range rep.stale {
			t.Errorf("allowlist entry %s -> %s (%s) no longer matches a forbidden import; the debt is paid, so delete the entry from allowlist_test.go",
				a.From, a.To, a.Debt)
		}
	})

	t.Run("Allowlist_WellFormed", func(t *testing.T) {
		for _, m := range rep.malformed {
			t.Errorf("allowlist_test.go: %s", m)
		}
		t.Logf("%d allow-listed edges", len(allowlist))
	})
}

// TestADR0001_LayeringCheckerDetectsFaults injects a forbidden edge, a stale
// allowlist entry and an unmapped package into a synthetic graph.
func TestADR0001_LayeringCheckerDetectsFaults(t *testing.T) {
	g := newGraph()
	for _, dir := range []string{
		"spinifex/foundation/widget", "spinifex/domains/alpha/thing",
		"spinifex/domains/beta/store", "spinifex/misc",
	} {
		g.pkgs[shortName(dir)] = dir
	}
	forbidden := edge{From: "foundation/widget", To: "domains/alpha/thing"}
	g.imports[forbidden] = "spinifex/foundation/widget/w.go:7"
	allowedEdge := edge{From: "domains/alpha/thing", To: "domains/beta/store"}
	g.imports[allowedEdge] = "spinifex/domains/alpha/thing/t.go:9"

	staleEntry := allowance{From: "domains/beta/store", To: "domains/alpha/thing", Debt: "synthetic", Reason: "stale"}
	rep := check(g, []allowance{
		{From: allowedEdge.From, To: allowedEdge.To, Debt: "synthetic", Reason: "recorded"},
		staleEntry,
	})

	if len(rep.unrecorded) != 1 || rep.unrecorded[0].edge != forbidden ||
		rep.unrecorded[0].rule != ruleByID("R1_FoundationImportsOnlyFoundation").id {
		t.Errorf("want only %s reported under R1, got %+v", forbidden, rep.unrecorded)
	}
	if len(rep.stale) != 1 || rep.stale[0] != staleEntry {
		t.Errorf("want stale entry %+v, got %+v", staleEntry, rep.stale)
	}
	if len(rep.unmapped) != 1 || !strings.HasPrefix(rep.unmapped[0], "misc ") {
		t.Errorf("want unmapped package misc, got %v", rep.unmapped)
	}
	var allowedHit bool
	for _, v := range rep.violations {
		allowedHit = allowedHit || v.edge == allowedEdge
	}
	if !allowedHit {
		t.Errorf("allow-listed cross-domain edge %s was not detected by R3, so the allowlist entry would read as stale", allowedEdge)
	}
}
