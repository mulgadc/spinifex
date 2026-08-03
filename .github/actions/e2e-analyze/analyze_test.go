package main

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// updateGolden lets `go test ./.github/actions/e2e-analyze/ -update` rewrite
// the expected analysis.md files when the rendering format changes
// intentionally. Day-to-day, leave it unset: tests diff against the on-disk
// snapshot to catch unintended drift in signature extraction or layout.
var updateGolden = flag.Bool("update", false, "rewrite testdata/*/analysis.md from current output")

// TestGoldenFixtures runs the full ParseFile + Render pipeline against each
// fixture directory under testdata/ and asserts the output matches the
// adjacent analysis.md snapshot. Each subdir is one named scenario; the
// fixture XMLs match the same junit-*.xml glob the production action uses
// so behaviour stays representative of CI.
func TestGoldenFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", name)
			matches, err := filepath.Glob(filepath.Join(dir, "junit-*.xml"))
			if err != nil {
				t.Fatalf("glob: %v", err)
			}
			sort.Strings(matches)
			if len(matches) == 0 {
				t.Fatalf("no junit-*.xml fixtures in %s", dir)
			}

			rep := Report{Title: "E2E failure analysis"}
			for _, p := range matches {
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("read %s: %v", p, err)
				}
				sr, err := ParseFile(p, data)
				if err != nil {
					t.Fatalf("parse %s: %v", p, err)
				}
				rep.Suites = append(rep.Suites, sr)
			}

			got := Render(rep)
			goldenPath := filepath.Join(dir, "analysis.md")
			if *updateGolden {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with -update to seed): %v", err)
			}
			if string(want) != got {
				t.Errorf("rendered output differs from %s\n--- want ---\n%s\n--- got ---\n%s",
					goldenPath, string(want), got)
			}
		})
	}
}

func TestSignatureCollapsesNoise(t *testing.T) {
	a := signature("describe-vpcs: InvalidParameterValue request-id: abc-123 vpc-deadbeefcafe1234")
	b := signature("describe-vpcs: InvalidParameterValue request-id: xyz-999 vpc-1111222233334444")
	if a != b {
		t.Errorf("signatures should collapse to same bucket:\n  a=%q\n  b=%q", a, b)
	}
	if !strings.Contains(a, "<id>") || !strings.Contains(a, "vpc-<id>") {
		t.Errorf("expected normalised placeholders in signature, got %q", a)
	}
}

func TestCascadeDetection(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Phase 5 must populate fix.InstanceID", true},
		{"Should NOT be empty", true},
		{"Expected value not to be nil", true},
		{"describe-vpcs: InvalidParameterValue", false},
		{"Eventually: condition not met within 3m0s", false},
	}
	for _, c := range cases {
		if got := isCascade(c.in); got != c.want {
			t.Errorf("isCascade(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestExtractErrorLine_MessagesWinsOverError(t *testing.T) {
	body := `    lifecycle_test.go:28:
        Error Trace:	tests/e2e/single/lifecycle_test.go:28
        Error:      	Should NOT be empty
        Messages:   	Phase 5 must populate fix.InstanceID
        Test:       	TestSingleNode/Phase5a_pre_ClusterStats`
	got := extractErrorLine(body)
	want := "Phase 5 must populate fix.InstanceID"
	if got != want {
		t.Errorf("extractErrorLine = %q, want %q", got, want)
	}
}

func TestExtractErrorLine_FallsBackToLastTestLine(t *testing.T) {
	body := "    vpc_test.go:227: Eventually: condition not met within 3m0s: [SSH handshake never completed]"
	got := extractErrorLine(body)
	if !strings.HasPrefix(got, "Eventually:") {
		t.Errorf("expected fallback to test-line content, got %q", got)
	}
}

// A test's cleanup logs after it has failed, so its lines are the last in the
// body — and the cleanup line here is one passing tests emit too.
func TestExtractErrorLine_SkipsCleanupLoggedAfterTheFailure(t *testing.T) {
	body := `    failure_test.go:42: DB instance rds-e2e-failure-1 reached status available
    failure_test.go:60: EventuallyErr: condition not met within 4m0s: rds-e2e-failure-1 status=available want=failed
    rds.go:364: db diagnostics rds-e2e-failure-1: no VM to capture a console from (<nil>)
    main_test.go:325: DB instance rds-e2e-failure-1 is gone`
	got := extractErrorLine(body)
	if !strings.HasPrefix(got, "EventuallyErr:") {
		t.Errorf("extractErrorLine = %q, want the assertion at failure_test.go:60", got)
	}
	if hint := extractFileHint(body); hint != "failure_test.go:60" {
		t.Errorf("extractFileHint = %q, want failure_test.go:60", hint)
	}
}

func TestExtractFileHint_PicksLastMatch(t *testing.T) {
	body := "ec2helpers.go:50: setup\n    vpc_test.go:227: Eventually: condition not met"
	got := extractFileHint(body)
	want := "vpc_test.go:227"
	if got != want {
		t.Errorf("extractFileHint = %q, want %q", got, want)
	}
}

func TestParseFile_SkipsEmptyParentFailure(t *testing.T) {
	// The synthetic "TestSingleNode" parent gets a <failure message="Failed"/>
	// with no body whenever any subtest fails. The analyzer must skip it so
	// it doesn't outrank the real first failure on XML order.
	xml := []byte(`<?xml version="1.0"?>
<testsuites><testsuite name="" tests="2" failures="2">
  <testcase name="TestX" time="1.0"><failure message="Failed"></failure></testcase>
  <testcase name="TestX/RealFailure" time="0.5"><failure message="Failed"><![CDATA[
    foo_test.go:10:
        Messages: real assertion message
]]></failure></testcase>
</testsuite></testsuites>`)
	sr, err := ParseFile("junit-x.xml", xml)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if sr.FailCount != 1 {
		t.Fatalf("FailCount = %d, want 1 (empty parent should be skipped)", sr.FailCount)
	}
	if sr.Root == nil || sr.Root.Name != "TestX/RealFailure" {
		t.Fatalf("expected real failure as root, got %+v", sr.Root)
	}
}

func TestParseFile_SkipsParentRolledUpBySubtests(t *testing.T) {
	// A parent that logs during cleanup gets a non-empty body, and a t.Log like
	// "DB instance … is gone" — which passing tests emit too — reads exactly
	// like an assertion site. Its subtest holds the real failure.
	xml := []byte(`<?xml version="1.0"?>
<testsuites><testsuite name="" tests="2" failures="2">
  <testcase name="TestX" time="1.5"><failure message="Failed"><![CDATA[
    main_test.go:325: DB instance rds-e2e-backup-1 is gone
]]></failure></testcase>
  <testcase name="TestX/RealFailure" time="0.5"><failure message="Failed"><![CDATA[
    foo_test.go:10:
        Messages: real assertion message
]]></failure></testcase>
</testsuite></testsuites>`)
	sr, err := ParseFile("junit-x.xml", xml)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if sr.FailCount != 1 {
		t.Fatalf("FailCount = %d, want 1 (the parent rollup should be skipped)", sr.FailCount)
	}
	if sr.Root == nil || sr.Root.Name != "TestX/RealFailure" {
		t.Fatalf("expected the subtest as root, got %+v", sr.Root)
	}
}

// A parent that fails on its own after its subtests passed is a real failure,
// and dropping it would lose the only record of it.
func TestParseFile_KeepsParentWithNoFailingSubtest(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?>
<testsuites><testsuite name="" tests="2" failures="1">
  <testcase name="TestX" time="1.5"><failure message="Failed"><![CDATA[
    foo_test.go:42:
        Messages: the parent asserted after its subtests
]]></failure></testcase>
  <testcase name="TestX/Passing" time="0.5"></testcase>
</testsuite></testsuites>`)
	sr, err := ParseFile("junit-x.xml", xml)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if sr.Root == nil || sr.Root.Name != "TestX" {
		t.Fatalf("expected the parent as root, got %+v", sr.Root)
	}
}

// suiteFixture writes a junit file plus its .start sidecar into a temp log dir
// and returns the parsed, start-file-corrected report for one suite.
func suiteFixture(t *testing.T, start string, xml []byte) SuiteReport {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test-x.start"), []byte(start), 0o644); err != nil {
		t.Fatalf("write start file: %v", err)
	}
	sr, err := ParseFile("junit-x.xml", xml)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	rep := Report{Suites: []SuiteReport{sr}}
	ApplySuiteStartFiles(&rep, dir)
	return rep.Suites[0]
}

// Parallel tests overlap, so summing the ones before a failure puts its start
// minutes past where it really was — and the journal slice cut from that window
// misses the failure entirely. The tell is the top-level tests taking longer in
// total than the suite's own wall clock.
func TestApplySuiteStartFiles_DropsTheOffsetWhenTheSuiteRanInParallel(t *testing.T) {
	// timestamp is when go-junit-report wrote the XML, i.e. the suite's end.
	s := suiteFixture(t, "2026-08-03T02:00:00Z", []byte(`<?xml version="1.0"?>
<testsuites><testsuite name="" tests="2" failures="1" timestamp="2026-08-03T02:02:00Z">
  <testcase name="TestSlow" time="100.0"></testcase>
  <testcase name="TestX" time="90.0"><failure message="Failed"><![CDATA[
    foo_test.go:10:
        Messages: real assertion message
]]></failure></testcase>
</testsuite></testsuites>`))
	if s.Root == nil {
		t.Fatal("expected a root failure")
	}
	if s.Root.SuiteSpan != 2*time.Minute {
		t.Errorf("SuiteSpan = %s, want 2m0s to mark the suite parallel", s.Root.SuiteSpan)
	}
	if want := time.Date(2026, 8, 3, 2, 0, 0, 0, time.UTC); !s.Root.StartAt.Equal(want) {
		t.Errorf("StartAt = %s, want the suite start %s rather than a summed offset", s.Root.StartAt, want)
	}
}

// The sequential case must keep its offsets: they are the only thing that makes
// a per-failure journal slice narrow enough to read.
func TestApplySuiteStartFiles_KeepsTheOffsetWhenTheSuiteRanSequentially(t *testing.T) {
	s := suiteFixture(t, "2026-08-03T02:00:00Z", []byte(`<?xml version="1.0"?>
<testsuites><testsuite name="" tests="2" failures="1" timestamp="2026-08-03T02:02:00Z">
  <testcase name="TestFirst" time="30.0"></testcase>
  <testcase name="TestX" time="90.0"><failure message="Failed"><![CDATA[
    foo_test.go:10:
        Messages: real assertion message
]]></failure></testcase>
</testsuite></testsuites>`))
	if s.Root == nil {
		t.Fatal("expected a root failure")
	}
	if s.Root.SuiteSpan != 0 {
		t.Errorf("SuiteSpan = %s, want 0 for a sequential suite", s.Root.SuiteSpan)
	}
	if want := time.Date(2026, 8, 3, 2, 0, 30, 0, time.UTC); !s.Root.StartAt.Equal(want) {
		t.Errorf("StartAt = %s, want the suite start plus 30s of preceding tests %s", s.Root.StartAt, want)
	}
}

func TestParseFile_CascadeFallbackWhenAllCascades(t *testing.T) {
	// If every failure is a cascade marker, the earliest one becomes root
	// so the report isn't blank.
	xml := []byte(`<?xml version="1.0"?>
<testsuites><testsuite name="" tests="2" failures="2">
  <testcase name="TestX/A" time="1.0"><failure message="Failed"><![CDATA[
    Messages: Phase 5 must populate fix.InstanceID
]]></failure></testcase>
  <testcase name="TestX/B" time="1.0"><failure message="Failed"><![CDATA[
    Messages: Phase 5 must populate fix.InstanceID
]]></failure></testcase>
</testsuite></testsuites>`)
	sr, err := ParseFile("junit-x.xml", xml)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if sr.Root == nil || sr.Root.Name != "TestX/A" {
		t.Fatalf("expected TestX/A as fallback root, got %+v", sr.Root)
	}
}

func TestRender_NoFailuresIsClean(t *testing.T) {
	out := Render(Report{
		Title: "E2E failure analysis",
		Suites: []SuiteReport{
			{File: "junit-single.xml", Label: "single", Total: 3, FailCount: 0},
		},
	})
	if !strings.Contains(out, "No failures") {
		t.Errorf("expected zero-failure banner, got:\n%s", out)
	}
}

// A cell that fails in provisioning writes junit with no testcases. Rendering
// the clean banner there reports "nothing ran" as "nothing wrong".
func TestRender_NoTestsRanIsNotClean(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suites []SuiteReport
	}{
		{"empty junit file", []SuiteReport{{File: "junit-baremetal.xml", Label: "baremetal", Total: 0, FailCount: 0}}},
		{"no junit files at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := Render(Report{Title: "E2E failure analysis", Suites: tc.suites})
			if strings.Contains(out, "No failures across any suite") {
				t.Errorf("clean banner rendered when no test ran, got:\n%s", out)
			}
			if !strings.Contains(out, "No tests ran") {
				t.Errorf("expected a no-tests-ran warning, got:\n%s", out)
			}
		})
	}
}
