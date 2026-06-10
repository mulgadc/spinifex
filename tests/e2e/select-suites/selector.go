// Package selectsuites derives the set of e2e suites a diff must exercise.
//
// Inputs: the service-interface manifest (docs/service-interfaces.yaml) and a
// list of repo-root-relative changed paths. Output: the suites to run, plus an
// optional per-suite -test.run regex (RUN_PATTERN) when AST analysis can prove
// a strict subtest subset is sufficient.
//
// Selection model (see docs/development/improvements/
// e2e-targeted-suite-selection.md):
//
//	changed_services = { s | s.path (or additional_paths) prefixes a changed file }
//	covered_services = changed_services plus the transitive closure of
//	                   *consumers*: x is covered if x.depends_on overlaps the
//	                   publishes set of an already-covered service.
//	must_run_suites  = { suite | suite.covers ∩ covered_services ≠ ∅ }
//
// Several special cases short-circuit to "run all" (infra/harness/workflow
// changes, ≥N services touched, submodule bumps) or to a single suite (a diff
// confined to one suite's own test dir). An empty result falls back to the
// default suites rather than skipping e2e entirely.
package selectsuites

import (
	"path"
	"sort"
	"strings"

	manifestcheck "github.com/mulgadc/spinifex/tests/e2e/manifest-check"
)

// Manifest is re-exported from manifestcheck so callers import one package.
type Manifest = manifestcheck.Manifest

// Result is the selector's output. AllSuites is true when a special case
// forced the full matrix; Suites then lists every manifest suite. RunPatterns
// maps a suite name to a -test.run regex when a strict subtest subset suffices;
// suites absent from the map run in full.
type Result struct {
	Suites      []string
	RunPatterns map[string]string
	AllSuites   bool
	Reason      string
}

// Config carries the hard-coded special-case rules. Defaults mirror the
// commented rules at the foot of service-interfaces.yaml; exposed as a struct
// so tests can pin them without depending on repo layout.
type Config struct {
	// InfraGlobs force "run all" when any changed file matches. Globs use
	// path.Match semantics per segment, with a trailing "/**" meaning
	// "this dir and everything under it".
	InfraGlobs []string
	// SubmodulePaths force "run all" when a changed file is at or under one
	// (a submodule pointer bump can hide a contract change).
	SubmodulePaths []string
	// ManyServicesThreshold: ≥ this many distinct changed services → run all.
	ManyServicesThreshold int
	// DefaultSuites run when selection is otherwise empty.
	DefaultSuites []string
}

// DefaultConfig returns the production special-case rules.
func DefaultConfig() Config {
	return Config{
		InfraGlobs: []string{
			"spinifex/services/nats/**",
			"tests/e2e/harness/**",
			"tests/e2e/lib/**",
			"tests/e2e/Makefile",
			"Makefile",
			".github/workflows/e2e.yml",
			".github/scripts/**",
		},
		SubmodulePaths: []string{
			"predastore",
			"viperblock",
		},
		ManyServicesThreshold: 3,
		DefaultSuites:         []string{"e2e-cert", "e2e-lb"},
	}
}

// Select computes the suites to run for changedFiles. changedFiles are
// repo-root-relative (as `git diff --name-only` emits inside the spinifex
// repo). runPatterns is an optional suite→(testName set) map from AST analysis;
// pass nil to skip subtest narrowing.
func Select(m *Manifest, cfg Config, changedFiles []string, perTest map[string]SuiteCoverage) Result {
	all := func(reason string) Result {
		names := suiteNames(m)
		return Result{Suites: names, AllSuites: true, Reason: reason}
	}

	if len(changedFiles) == 0 {
		return Result{Suites: append([]string(nil), cfg.DefaultSuites...), Reason: "empty diff → default suites"}
	}

	// 1. Force-all special cases take precedence over everything.
	for _, f := range changedFiles {
		if g := matchInfra(f, cfg.InfraGlobs); g != "" {
			return all("infra path changed: " + f + " (" + g + ")")
		}
		if s := matchSubmodule(f, cfg.SubmodulePaths); s != "" {
			return all("submodule pointer changed: " + s)
		}
	}

	// 2. A diff confined to a single suite's own test dir runs only that suite.
	if name, ok := loneSuiteDir(m, changedFiles); ok {
		return Result{Suites: []string{name}, Reason: "diff confined to suite dir " + name}
	}

	// 3. Map changed files to changed services, then take the consumer closure.
	changedSvc := changedServices(m, changedFiles)
	if len(changedSvc) >= cfg.ManyServicesThreshold {
		return all("≥3 services touched")
	}
	covered := consumerClosure(m, changedSvc)

	// 4. Suites whose covers intersect the covered service set.
	suites := suitesForServices(m, covered)
	if len(suites) == 0 {
		return Result{Suites: append([]string(nil), cfg.DefaultSuites...), Reason: "no suite matched → default suites"}
	}

	res := Result{Suites: suites, Reason: "service-mapped: " + strings.Join(sortedKeys(covered), ",")}

	// 5. Best-effort subtest narrowing: for each selected suite with per-test
	// coverage data, keep only the tests touching a covered service. Emit a
	// RUN_PATTERN only when that is a strict, non-empty subset.
	if len(perTest) > 0 {
		res.RunPatterns = runPatterns(perTest, suites, covered)
	}
	return res
}

// changedServices returns the set of service names whose path or
// additional_paths prefix at least one changed file.
func changedServices(m *Manifest, changed []string) map[string]bool {
	out := map[string]bool{}
	for name, svc := range m.Services {
		for _, p := range append([]string{svc.Path}, svc.AdditionalPaths...) {
			if p == "" {
				continue
			}
			for _, f := range changed {
				if underDir(f, p) {
					out[name] = true
				}
			}
		}
	}
	return out
}

// consumerClosure grows seed with every service that depends_on a subject an
// already-covered service handles — whether that service *publishes* the
// subject (produces data the dependant consumes) or *subscribes* to it (handles
// requests the dependant sends). Both mean "if the covered service changes, the
// dependant may break". Iterates to a fixpoint.
func consumerClosure(m *Manifest, seed map[string]bool) map[string]bool {
	covered := map[string]bool{}
	for k := range seed {
		covered[k] = true
	}
	for {
		added := false
		for name, svc := range m.Services {
			if covered[name] {
				continue
			}
			for cname := range covered {
				c := m.Services[cname]
				if subjectsOverlap(svc.DependsOn, c.Publishes) ||
					subjectsOverlap(svc.DependsOn, c.Subscribes) {
					covered[name] = true
					added = true
					break
				}
			}
		}
		if !added {
			return covered
		}
	}
}

// suitesForServices returns the sorted suite names whose covers list intersects
// the covered service set. A suite with covers [all] always matches.
func suitesForServices(m *Manifest, covered map[string]bool) []string {
	var out []string
	for name, s := range m.Suites {
		match := false
		for _, c := range s.Covers {
			if c == "all" || covered[c] {
				match = true
				break
			}
		}
		if match {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// loneSuiteDir reports the suite when every changed file lives under exactly
// one suite's path and that path is not a shared dir (harness/lib are infra,
// handled earlier).
func loneSuiteDir(m *Manifest, changed []string) (string, bool) {
	hit := ""
	for _, f := range changed {
		owner := ""
		for name, s := range m.Suites {
			if s.Path != "" && underDir(f, s.Path) {
				owner = name
				break
			}
		}
		if owner == "" {
			return "", false // a non-suite file is in the diff
		}
		if hit == "" {
			hit = owner
		} else if hit != owner {
			return "", false // spans ≥2 suites
		}
	}
	return hit, hit != ""
}

func matchInfra(f string, globs []string) string {
	for _, g := range globs {
		if globMatch(g, f) {
			return g
		}
	}
	return ""
}

func matchSubmodule(f string, subs []string) string {
	for _, s := range subs {
		if f == s || underDir(f, s) {
			return s
		}
	}
	return ""
}

// underDir reports whether file f is dir itself or sits beneath it. Both are
// slash-separated repo-relative paths.
func underDir(f, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return f == dir || strings.HasPrefix(f, dir+"/")
}

// globMatch supports a trailing "/**" (this dir and all descendants) plus
// per-segment path.Match for the rest. Without "/**" it is an exact or
// path.Match comparison.
func globMatch(pattern, f string) bool {
	if before, ok := strings.CutSuffix(pattern, "/**"); ok {
		return underDir(f, before)
	}
	if ok, _ := path.Match(pattern, f); ok {
		return true
	}
	return pattern == f
}

// subjectsOverlap reports whether any subject in a matches any in b under
// trailing-wildcard semantics (either side may end in ".*").
func subjectsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if subjectMatch(x, y) {
				return true
			}
		}
	}
	return false
}

// subjectMatch compares two subjects, each optionally ending ".*". A wildcard
// matches anything sharing its prefix; two wildcards match if one prefix
// contains the other.
func subjectMatch(x, y string) bool {
	xw := strings.HasSuffix(x, ".*")
	yw := strings.HasSuffix(y, ".*")
	xp := strings.TrimSuffix(x, ".*")
	yp := strings.TrimSuffix(y, ".*")
	switch {
	case xw && yw:
		return xp == yp || strings.HasPrefix(xp, yp+".") || strings.HasPrefix(yp, xp+".")
	case xw:
		return y == xp || strings.HasPrefix(y, xp+".")
	case yw:
		return x == yp || strings.HasPrefix(x, yp+".")
	default:
		return x == y
	}
}

func suiteNames(m *Manifest) []string {
	out := make([]string, 0, len(m.Suites))
	for k := range m.Suites {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(s map[string]bool) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
