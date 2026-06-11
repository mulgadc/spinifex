package manifestlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	manifestcheck "github.com/mulgadc/spinifex/tests/e2e/manifest-check"
)

// writeFile creates path (with parents) under root and returns nothing.
func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureLint_FlagsDirectCreate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tests/e2e/single/x_test.go", `package single
func TestA(t *T) {
	c.EC2.RunInstances(nil)
	c.ELBv2.CreateLoadBalancer(nil)
	harness.EnsureInstance(t, fx, spec) // not a direct create
}
`)
	res, err := FixtureLint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 2 {
		t.Fatalf("want 2 violations, got %d: %+v", len(res.Violations), res.Violations)
	}
	joined := ""
	for _, v := range res.Violations {
		joined += v.Msg + "\n"
	}
	for _, want := range []string{"RunInstances", "CreateLoadBalancer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestFixtureLint_AllowMarkerExempts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tests/e2e/single/x_test.go", `package single
func TestA(t *T) {
	c.EC2.RunInstances(nil) // e2e:allow-create
	// e2e:allow-create
	c.EC2.CreateVpc(nil)
}
`)
	res, err := FixtureLint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("allow-create should exempt both, got %d: %+v", len(res.Violations), res.Violations)
	}
}

func TestFixtureLint_IgnoresNonClientCreate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tests/e2e/single/x_test.go", `package single
func TestA(t *T) {
	os.CreateVpc(nil)       // wrong receiver field
	createVpc()             // local helper, not a client call
	c.Other.RunInstances(0) // not an AWS client field
}
`)
	res, err := FixtureLint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("want 0, got %d: %+v", len(res.Violations), res.Violations)
	}
}

func TestFixtureLint_ExcludesHarness(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tests/e2e/harness/fixtures_test.go", `package harness
func TestEnsure(t *T) { c.EC2.RunInstances(nil) }
`)
	res, err := FixtureLint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("harness dir must be excluded, got %d", len(res.Violations))
	}
}

func TestFixtureLint_OrdinalKeysStable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tests/e2e/single/x_test.go", `package single
func TestA(t *T) {
	c.EC2.RunInstances(1)
	c.EC2.RunInstances(2)
}
`)
	res, err := FixtureLint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 2 {
		t.Fatalf("want 2, got %d", len(res.Violations))
	}
	keys := map[string]bool{}
	for _, v := range res.Violations {
		keys[v.Key] = true
	}
	if !keys["create\ttests/e2e/single/x_test.go\tRunInstances\t#1"] ||
		!keys["create\ttests/e2e/single/x_test.go\tRunInstances\t#2"] {
		t.Fatalf("ordinal keys wrong: %v", keys)
	}
}

func testManifest() *manifestcheck.Manifest {
	return &manifestcheck.Manifest{
		Version: 1,
		Services: map[string]manifestcheck.Service{
			"daemon": {
				Path:       "src/daemon",
				Subscribes: []string{"ec2.RunInstances", "vpc.*"},
				Publishes:  []string{"ebs.mount"},
			},
		},
	}
}

func TestSubjectLint_FlagsUndeclared(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/daemon/d.go", `package daemon
type natsSub struct{ topic string; h func(); q string }
func reg() {
	subs := []natsSub{
		{"ec2.RunInstances", nil, ""}, // declared
		{"ec2.SecretSubject", nil, ""}, // NOT declared
	}
	nc.Publish("vpc.create", nil)  // covered by vpc.* wildcard
	nc.Request("ec2.Mystery", nil) // undeclared
	_ = subs
}
`)
	res, err := SubjectLint(root, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range res.Violations {
		got = append(got, v.Msg)
	}
	joined := strings.Join(got, "\n")
	if len(res.Violations) != 2 {
		t.Fatalf("want 2 undeclared, got %d:\n%s", len(res.Violations), joined)
	}
	for _, want := range []string{"ec2.SecretSubject", "ec2.Mystery"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "vpc.create") {
		t.Errorf("vpc.create should be covered by vpc.* wildcard")
	}
}

func TestSubjectLint_SkipsDynamicAndOutOfScope(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/daemon/d.go", `package daemon
func reg() {
	nc.Subscribe(fmt.Sprintf("ec2.cmd.%s", id), nil) // dynamic, skipped
	nc.Subscribe("test.internal.reply", nil)         // out-of-scope prefix
	nc.Publish("ec2.start: log line not a subject", nil)
}
`)
	res, err := SubjectLint(root, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("dynamic/out-of-scope must not violate, got %d: %+v", len(res.Violations), res.Violations)
	}
}

func TestFilter_NewVsStale(t *testing.T) {
	cands := []Violation{{Key: "a", Msg: "A"}, {Key: "b", Msg: "B"}}
	baseline := map[string]bool{"a": true, "c": true}
	newV, stale := Filter(cands, baseline)
	if len(newV) != 1 || newV[0].Key != "b" {
		t.Fatalf("want new=[b], got %+v", newV)
	}
	if len(stale) != 1 || stale[0] != "c" {
		t.Fatalf("want stale=[c], got %+v", stale)
	}
}

func TestBaselineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	if err := WriteBaseline(path, []string{"b", "a", "a"}); err != nil {
		t.Fatal(err)
	}
	set, err := LoadBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 2 || !set["a"] || !set["b"] {
		t.Fatalf("round-trip wrong: %v", set)
	}
}

// TestRealRepo_NoNewViolations guards the checked-in baseline: the committed
// baseline.txt must keep the live repo green so preflight stays deterministic.
func TestRealRepo_NoNewViolations(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifestcheck.Load(filepath.Join(root, "docs/service-interfaces.yaml"))
	if err != nil {
		t.Skipf("manifest unavailable: %v", err)
	}
	fix, err := FixtureLint(root)
	if err != nil {
		t.Fatal(err)
	}
	subj, err := SubjectLint(root, m)
	if err != nil {
		t.Fatal(err)
	}
	cands := append(append([]Violation{}, fix.Violations...), subj.Violations...)
	base, err := LoadBaseline(filepath.Join(root, "tests/e2e/manifest-lint/baseline.txt"))
	if err != nil {
		t.Fatal(err)
	}
	newV, _ := Filter(cands, base)
	if len(newV) != 0 {
		for _, v := range newV {
			t.Errorf("new violation not in baseline: %s", v.Msg)
		}
		t.Fatalf("%d new violation(s); run `make manifest-lint-update` if intended", len(newV))
	}
}
