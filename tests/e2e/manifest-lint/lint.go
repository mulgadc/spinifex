// Package manifestlint implements the e2e manifest drift guards (Bead 5 of
// docs/development/improvements/e2e-targeted-suite-selection.md):
//
//   - Fixture lint: every direct resource-create AWS call in a suite test
//     (e.g. c.EC2.RunInstances) should go through a harness.Ensure* fixture so
//     setup is shared and reusable. Pre-existing call sites are captured in a
//     checked-in baseline; the lint fails only on NEW direct creates beyond the
//     baseline. Intentional create-path assertions opt out with an inline
//     // e2e:allow-create comment.
//
//   - Subject lint: NATS subject string literals referenced in service source
//     (the natsSub{...} registry and literal Subscribe/Publish/Request args)
//     must be declared in docs/service-interfaces.yaml. Code subjects absent
//     from the manifest are violations (baseline-ratcheted). Manifest subjects
//     with no static reference are reported as warnings, not failures, because
//     many subjects are produced dynamically (fmt.Sprintf) and cannot be
//     matched statically.
//
// Both checks share one baseline file and one CLI (cmd/manifest-lint), wired
// into `make manifest-lint` and `make preflight`.
package manifestlint

import (
	"bufio"
	"os"
	"sort"
	"strings"
)

// Violation is a single drift finding. Key is the stable baseline identity
// (no line numbers, so it survives unrelated edits); Msg is the human-readable
// line shown to the developer.
type Violation struct {
	Key string
	Msg string
}

// Result aggregates a lint run before baseline filtering is applied.
type Result struct {
	Violations []Violation // candidate findings (pre-baseline)
	Warnings   []string    // advisory only, never fail the build
}

func (r *Result) add(key, msg string) {
	r.Violations = append(r.Violations, Violation{Key: key, Msg: msg})
}

func (r *Result) warn(msg string) { r.Warnings = append(r.Warnings, msg) }

// LoadBaseline reads a baseline file into a set of keys. A missing file is an
// empty baseline (every violation is then "new").
func LoadBaseline(path string) (map[string]bool, error) {
	set := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		set[line] = true
	}
	return set, sc.Err()
}

// WriteBaseline writes keys (sorted, deduplicated) to path with a header.
func WriteBaseline(path string, keys []string) error {
	uniq := map[string]bool{}
	for _, k := range keys {
		uniq[k] = true
	}
	out := make([]string, 0, len(uniq))
	for k := range uniq {
		out = append(out, k)
	}
	sort.Strings(out)

	var b strings.Builder
	b.WriteString("# manifest-lint baseline — pre-existing drift accepted by the ratchet.\n")
	b.WriteString("# Regenerate with: make manifest-lint-update. Migrate entries away over time.\n")
	b.WriteString("# Lines: \"create\\t<file>\\t<API>\\t#<n>\" or \"subject\\t<file>\\t<subject>\".\n")
	for _, k := range out {
		b.WriteString(k)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// Filter splits candidate violations against a baseline: "new" are findings not
// present in the baseline (these fail the build); "stale" are baseline keys no
// longer observed (advisory — the baseline can be slimmed).
func Filter(cands []Violation, baseline map[string]bool) (added []Violation, stale []string) {
	observed := map[string]bool{}
	for _, v := range cands {
		observed[v.Key] = true
		if !baseline[v.Key] {
			added = append(added, v)
		}
	}
	for k := range baseline {
		if !observed[k] {
			stale = append(stale, k)
		}
	}
	sort.Slice(added, func(i, j int) bool { return added[i].Msg < added[j].Msg })
	sort.Strings(stale)
	return added, stale
}

// AllKeys returns the stable keys of every candidate violation, for seeding or
// regenerating the baseline.
func AllKeys(cands []Violation) []string {
	keys := make([]string, 0, len(cands))
	for _, v := range cands {
		keys = append(keys, v.Key)
	}
	return keys
}

func tab(parts ...string) string { return strings.Join(parts, "\t") }
