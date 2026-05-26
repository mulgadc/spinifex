package main

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
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
