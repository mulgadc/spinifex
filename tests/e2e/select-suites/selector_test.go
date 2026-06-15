package selectsuites

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	manifestcheck "github.com/mulgadc/spinifex/tests/e2e/manifest-check"
)

// testManifest mirrors the essentials of docs/service-interfaces.yaml: the five
// daemon services plus cert/lb/single/ddil/reboot suites and the fixtures
// block. Kept inline so the unit tests do not depend on the real manifest
// drifting.
func testManifest() *Manifest {
	return &manifestcheck.Manifest{
		Version: 1,
		Services: map[string]manifestcheck.Service{
			"awsgw": {
				Path:       "spinifex/services/awsgw",
				Publishes:  []string{"ec2.*", "elbv2.*", "acm.*", "iam.*"},
				DependsOn:  []string{"ec2.*", "elbv2.*", "acm.*", "iam.*"},
				Subscribes: []string{},
			},
			"spinifex_daemon": {
				Path:            "spinifex/services/spinifex",
				AdditionalPaths: []string{"spinifex/daemon", "spinifex/handlers/ec2"},
				Subscribes:      []string{"ec2.RunInstances", "elbv2.CreateLoadBalancer"},
				Publishes:       []string{"vpc.create", "ebs.mount", "s3.*", "iam.account.created"},
				DependsOn:       []string{"vpc.*", "ebs.*", "s3.*"},
			},
			"vpcd": {
				Path:       "spinifex/vpcd",
				Subscribes: []string{"vpc.create", "vpc.delete"},
				Publishes:  []string{"vpc.port-status"},
				DependsOn:  []string{},
			},
			"viperblockd": {
				Path:       "spinifex/services/viperblockd",
				Subscribes: []string{"ebs.mount", "ebs.unmount"},
				Publishes:  []string{"ebs.mount.response"},
				DependsOn:  []string{},
			},
			"predastore": {
				Path:       "spinifex/services/predastore",
				Subscribes: []string{"s3.*"},
				Publishes:  []string{},
				DependsOn:  []string{},
			},
		},
		Fixtures: map[string]manifestcheck.Fixture{
			"EnsureDefaultVPC": {Services: []string{"vpcd", "spinifex_daemon"}},
			"EnsureInstance":   {Services: []string{"spinifex_daemon", "viperblockd", "vpcd"}},
			"EnsureVolume":     {Services: []string{"spinifex_daemon", "viperblockd"}},
		},
		Suites: map[string]manifestcheck.Suite{
			"e2e-cert":   {Path: "tests/e2e/cert", Covers: []string{"awsgw", "spinifex_daemon", "vpcd", "predastore"}},
			"e2e-lb":     {Path: "tests/e2e/lb", Covers: []string{"awsgw", "spinifex_daemon", "vpcd", "viperblockd", "predastore"}},
			"e2e-single": {Path: "tests/e2e/single", Covers: []string{"awsgw", "spinifex_daemon", "vpcd", "viperblockd", "predastore"}},
			"e2e-ddil":   {Path: "tests/e2e/ddil", Covers: []string{"awsgw", "spinifex_daemon", "vpcd", "viperblockd", "predastore"}},
			"e2e-reboot": {Path: "tests/e2e/reboot", Covers: []string{"spinifex_daemon", "viperblockd"}},
		},
	}
}

func selectSuites(t *testing.T, changed []string) Result {
	t.Helper()
	return Select(testManifest(), DefaultConfig(), changed, nil)
}

func wantSuites(t *testing.T, got Result, want ...string) {
	t.Helper()
	g := append([]string(nil), got.Suites...)
	sort.Strings(g)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("suites = %v, want %v (reason: %s)", g, w, got.Reason)
	}
}

func TestSelect_EmptyDiff_Default(t *testing.T) {
	got := selectSuites(t, nil)
	wantSuites(t, got, "e2e-cert", "e2e-lb")
}

func TestSelect_VPCDChange_ClosureToAWSGW(t *testing.T) {
	// vpcd → spinifex_daemon (depends_on vpc.*) → awsgw (depends_on iam.* of
	// daemon's iam.account.created). reboot does not cover awsgw/vpcd but does
	// cover spinifex_daemon, so it is included; every suite covers the daemon.
	got := selectSuites(t, []string{"spinifex/vpcd/topology.go"})
	wantSuites(t, got, "e2e-cert", "e2e-lb", "e2e-single", "e2e-ddil", "e2e-reboot")
}

func TestSelect_PredastoreChange_DiscriminatesReboot(t *testing.T) {
	// predastore publishes nothing, but spinifex_daemon depends_on s3.* which
	// predastore subscribes → daemon + awsgw covered. reboot covers neither
	// predastore nor awsgw, only daemon/viperblockd → still matches via daemon.
	// Use a service nobody depends on to truly discriminate: see vpcd-isolated.
	got := selectSuites(t, []string{"spinifex/services/predastore/adapter.go"})
	wantSuites(t, got, "e2e-cert", "e2e-lb", "e2e-single", "e2e-ddil", "e2e-reboot")
}

func TestSelect_AdditionalPaths(t *testing.T) {
	// handlers/ec2 is an additional_path of spinifex_daemon.
	got := selectSuites(t, []string{"spinifex/handlers/ec2/runinstances.go"})
	if len(got.Suites) == 0 || got.AllSuites {
		t.Fatalf("expected service-mapped selection, got %+v", got)
	}
}

func TestSelect_HarnessChange_ForcesAll(t *testing.T) {
	got := selectSuites(t, []string{"tests/e2e/harness/fixtures.go"})
	if !got.AllSuites {
		t.Fatalf("harness change must force all; got %+v", got)
	}
	wantSuites(t, got, "e2e-cert", "e2e-lb", "e2e-single", "e2e-ddil", "e2e-reboot")
}

func TestSelect_WorkflowChange_ForcesAll(t *testing.T) {
	got := selectSuites(t, []string{".github/workflows/e2e.yml"})
	if !got.AllSuites {
		t.Fatalf("workflow change must force all; got %+v", got)
	}
}

func TestSelect_NatsInfra_ForcesAll(t *testing.T) {
	got := selectSuites(t, []string{"spinifex/services/nats/cluster.go"})
	if !got.AllSuites {
		t.Fatalf("nats change must force all; got %+v", got)
	}
}

func TestSelect_ManyServices_ForcesAll(t *testing.T) {
	got := selectSuites(t, []string{
		"spinifex/vpcd/x.go",
		"spinifex/services/viperblockd/y.go",
		"spinifex/services/awsgw/z.go",
	})
	if !got.AllSuites {
		t.Fatalf("≥3 services must force all; got %+v", got)
	}
}

func TestSelect_LoneSuiteDir(t *testing.T) {
	got := selectSuites(t, []string{"tests/e2e/cert/cert_test.go"})
	wantSuites(t, got, "e2e-cert")
	if got.AllSuites {
		t.Fatalf("lone suite dir must not force all")
	}
}

func TestSelect_SuiteDirPlusSource_NotLone(t *testing.T) {
	// A suite test edit alongside a service edit is not a lone-suite diff; it
	// resolves through the service map (and may add suites).
	got := selectSuites(t, []string{"tests/e2e/cert/cert_test.go", "spinifex/vpcd/x.go"})
	if len(got.Suites) < 2 {
		t.Fatalf("expected service-mapped multi-suite, got %+v", got)
	}
}

func TestSelect_Submodule_ForcesAll(t *testing.T) {
	got := selectSuites(t, []string{"viperblock"})
	if !got.AllSuites {
		t.Fatalf("submodule pointer change must force all; got %+v", got)
	}
}

func TestSelect_UnmappedDocOnly_Default(t *testing.T) {
	// A docs-only change maps to no service and no suite dir → default suites.
	got := selectSuites(t, []string{"docs/DESIGN.md"})
	wantSuites(t, got, "e2e-cert", "e2e-lb")
}

func TestSubjectMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"vpc.*", "vpc.port-status", true},
		{"vpc.create", "vpc.*", true},
		{"vpc.*", "vpc.*", true},
		{"vpc.*", "vpcd.create", false}, // prefix must hit a segment boundary
		{"ebs.*", "ebs.mount.response", true},
		{"s3.*", "s3.*", true},
		{"ec2.RunInstances", "ec2.RunInstances", true},
		{"ec2.RunInstances", "ec2.Terminate", false},
		{"vpc.create.*", "vpc.*", true},
	}
	for _, c := range cases {
		if got := subjectMatch(c.a, c.b); got != c.want {
			t.Errorf("subjectMatch(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// --- AST scan -------------------------------------------------------------

func TestScanSuites_PerTestCoverage(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tests", "e2e", "faux")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := `//go:build e2e

package faux

import "testing"

func helperVPC(t *testing.T) string { return harness.EnsureDefaultVPC(t, nil) }

func TestVPCOnly(t *testing.T) {
	_ = helperVPC(t) // transitive Ensure via local helper
}

func TestInstance(t *testing.T) {
	_ = harness.EnsureInstance(t, nil, spec)
}

func TestReadOnly(t *testing.T) {
	// no Ensure* calls → unknown coverage, always kept
}
`
	if err := os.WriteFile(filepath.Join(dir, "faux_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	m := testManifest()
	m.Suites = map[string]manifestcheck.Suite{
		"e2e-faux": {Path: "tests/e2e/faux", Covers: []string{"vpcd"}},
	}

	cov, err := ScanSuites(root, m)
	if err != nil {
		t.Fatalf("ScanSuites: %v", err)
	}
	faux, ok := cov["e2e-faux"]
	if !ok {
		t.Fatalf("no coverage for e2e-faux: %+v", cov)
	}
	// TestVPCOnly reaches EnsureDefaultVPC via helperVPC → {vpcd, spinifex_daemon}.
	if !reflect.DeepEqual(faux.Tests["TestVPCOnly"], []string{"spinifex_daemon", "vpcd"}) {
		t.Errorf("TestVPCOnly services = %v", faux.Tests["TestVPCOnly"])
	}
	// TestInstance → EnsureInstance → {spinifex_daemon, viperblockd, vpcd}.
	if !reflect.DeepEqual(faux.Tests["TestInstance"], []string{"spinifex_daemon", "viperblockd", "vpcd"}) {
		t.Errorf("TestInstance services = %v", faux.Tests["TestInstance"])
	}
	// TestReadOnly has no fixtures.
	if len(faux.Tests["TestReadOnly"]) != 0 {
		t.Errorf("TestReadOnly should have no services, got %v", faux.Tests["TestReadOnly"])
	}
}

func TestRunPatterns_StrictSubset(t *testing.T) {
	perTest := map[string]SuiteCoverage{
		"e2e-single": {Tests: map[string][]string{
			"TestVPCCRUD":     {"vpcd", "spinifex_daemon"},
			"TestVolume":      {"viperblockd", "spinifex_daemon"},
			"TestReadOnly":    {}, // always kept
			"TestSnapshotBak": {"viperblockd", "spinifex_daemon", "predastore"},
		}},
	}
	// covered excludes spinifex_daemon: only a vpcd-confined closure narrows,
	// since almost every test transitively touches the daemon.
	covered := map[string]bool{"vpcd": true}
	got := runPatterns(perTest, []string{"e2e-single"}, covered)
	// Keeps TestVPCCRUD (vpcd) + TestReadOnly (unknown). Drops Volume/Snapshot.
	want := "^(TestReadOnly|TestVPCCRUD)$"
	if got["e2e-single"] != want {
		t.Fatalf("RUN_PATTERN = %q, want %q", got["e2e-single"], want)
	}
}

func TestRunPatterns_NoNarrowingWhenAllMatch(t *testing.T) {
	perTest := map[string]SuiteCoverage{
		"e2e-single": {Tests: map[string][]string{
			"TestA": {"vpcd"},
			"TestB": {"vpcd"},
		}},
	}
	covered := map[string]bool{"vpcd": true}
	got := runPatterns(perTest, []string{"e2e-single"}, covered)
	if _, ok := got["e2e-single"]; ok {
		t.Fatalf("expected no pattern when all tests match, got %v", got)
	}
}

// Guard: the real manifest loads and Select runs against it without panicking.
func TestSelect_RealManifest_Smoke(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	m, err := manifestcheck.Load(filepath.Join(root, "docs", "service-interfaces.yaml"))
	if err != nil {
		t.Fatalf("load real manifest: %v", err)
	}
	res := Select(m, DefaultConfig(), []string{"spinifex/vpcd/topology.go"}, nil)
	if len(res.Suites) == 0 {
		t.Fatalf("real manifest vpcd change selected no suites")
	}
}
