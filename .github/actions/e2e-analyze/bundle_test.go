package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSliceJournal_KeepsOnlyWindow(t *testing.T) {
	src := filepath.Join(t.TempDir(), "journal.log")
	dst := filepath.Join(t.TempDir(), "out.log")
	body := strings.Join([]string{
		"2026-05-19T12:32:30+1000 host spinifex-daemon[1]: pre",
		"  continuation of pre",
		"2026-05-19T12:32:35+1000 host spinifex-daemon[1]: IN window",
		"  multiline inside window",
		"2026-05-19T12:32:45+1000 host spinifex-daemon[1]: post",
		"",
	}, "\n")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	start, _ := time.Parse(time.RFC3339, "2026-05-19T12:32:33+10:00")
	end, _ := time.Parse(time.RFC3339, "2026-05-19T12:32:40+10:00")
	n, err := SliceJournal(src, start, end, dst)
	if err != nil {
		t.Fatalf("SliceJournal: %v", err)
	}
	if n != 2 {
		t.Fatalf("written = %d, want 2", n)
	}
	got, _ := os.ReadFile(dst)
	if !strings.Contains(string(got), "IN window") || !strings.Contains(string(got), "multiline inside window") {
		t.Errorf("missing in-window content:\n%s", got)
	}
	if strings.Contains(string(got), "pre") || strings.Contains(string(got), "post") {
		t.Errorf("kept out-of-window content:\n%s", got)
	}
}

func TestSliceJournal_AcceptsRFC3339ColonOffset(t *testing.T) {
	// systemd's `journalctl --output=short-iso` ships RFC3339 timestamps
	// (offset includes a colon, e.g. "+10:00") on most versions. Earlier
	// regex only matched "+1000" — make sure both forms work.
	src := filepath.Join(t.TempDir(), "journal.log")
	dst := filepath.Join(t.TempDir(), "out.log")
	body := strings.Join([]string{
		"2026-05-19T12:32:30+10:00 host spinifex-daemon[1]: pre",
		"2026-05-19T12:32:35+10:00 host spinifex-daemon[1]: IN window",
		"2026-05-19T12:32:50+10:00 host spinifex-daemon[1]: post",
		"",
	}, "\n")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	start, _ := time.Parse(time.RFC3339, "2026-05-19T12:32:33+10:00")
	end, _ := time.Parse(time.RFC3339, "2026-05-19T12:32:40+10:00")
	n, err := SliceJournal(src, start, end, dst)
	if err != nil {
		t.Fatalf("SliceJournal: %v", err)
	}
	if n != 1 {
		t.Fatalf("written = %d, want 1", n)
	}
	got, _ := os.ReadFile(dst)
	if !strings.Contains(string(got), "IN window") {
		t.Errorf("missing in-window content:\n%s", got)
	}
}

func TestSliceJournal_MissingSrcIsNotError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.log")
	n, err := SliceJournal("/no/such/file", time.Now(), time.Now(), dst)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if n != 0 {
		t.Errorf("written = %d, want 0", n)
	}
}

func TestSliceTestLog_PicksOneTestcase(t *testing.T) {
	src := filepath.Join(t.TempDir(), "test.log")
	dst := filepath.Join(t.TempDir(), "out.log")
	body := strings.Join([]string{
		"=== RUN   TestSingleNode",
		"  before subtests",
		"=== RUN   TestSingleNode/Phase5_LaunchInstance",
		"    runinstances",
		"    ec2helpers.go:164: describe-vpcs: InvalidParameterValue",
		"--- FAIL: TestSingleNode/Phase5_LaunchInstance (2.50s)",
		"=== RUN   TestSingleNode/Phase6_Volumes",
		"    other content",
		"--- FAIL: TestSingleNode/Phase6_Volumes (0.10s)",
		"--- FAIL: TestSingleNode (10.0s)",
		"",
	}, "\n")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	n, err := SliceTestLog(src, "TestSingleNode/Phase5_LaunchInstance", dst)
	if err != nil {
		t.Fatalf("SliceTestLog: %v", err)
	}
	if n != 4 {
		t.Fatalf("written = %d, want 4 (RUN + 2 inner + FAIL)", n)
	}
	got, _ := os.ReadFile(dst)
	s := string(got)
	if !strings.Contains(s, "describe-vpcs: InvalidParameterValue") {
		t.Errorf("missing failure line:\n%s", s)
	}
	if strings.Contains(s, "Phase6_Volumes") || strings.Contains(s, "before subtests") {
		t.Errorf("leaked sibling test content:\n%s", s)
	}
}

func TestSliceTestLog_UnknownTestcaseIsEmpty(t *testing.T) {
	src := filepath.Join(t.TempDir(), "test.log")
	dst := filepath.Join(t.TempDir(), "out.log")
	if err := os.WriteFile(src, []byte("=== RUN   TestX\n--- PASS: TestX (0.0s)\n"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	n, err := SliceTestLog(src, "TestY", dst)
	if err != nil {
		t.Fatalf("SliceTestLog: %v", err)
	}
	if n != 0 {
		t.Errorf("written = %d, want 0", n)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("dst not created: %v", err)
	}
}

// TestWriteBundles_EndToEnd builds a fake artifact dir with a junit XML,
// a journal log, a test log, and a per-test artifact subdir, then runs
// the full ParseFile → WriteBundles pipeline and asserts the bundle dir
// contains everything the report links to.
func TestWriteBundles_EndToEnd(t *testing.T) {
	dir := t.TempDir()

	junit := `<?xml version="1.0"?>
<testsuites><testsuite name="" tests="2" failures="1" timestamp="2026-05-19T12:32:30+10:00" time="3.0">
  <testcase name="TestSingleNode/Phase5_LaunchInstance" time="2.500">
    <failure message="Failed"><![CDATA[
    ec2helpers.go:164: describe-vpcs: InvalidParameterValue (status 400)
        Error Trace: tests/e2e/harness/ec2helpers.go:164
        Test: TestSingleNode/Phase5_LaunchInstance]]></failure>
  </testcase>
</testsuite></testsuites>`
	if err := os.WriteFile(filepath.Join(dir, "junit-single.xml"), []byte(junit), 0o644); err != nil {
		t.Fatalf("write junit: %v", err)
	}

	journal := strings.Join([]string{
		"2026-05-19T12:32:20+1000 host spinifex-daemon[1]: too-early",
		"2026-05-19T12:32:30+1000 host spinifex-daemon[1]: in-window-start",
		"2026-05-19T12:32:32+1000 host spinifex-daemon[1]: describe-vpcs filter=is_default",
		"2026-05-19T12:32:50+1000 host spinifex-daemon[1]: too-late",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "spinifex-journal.log"), []byte(journal), 0o644); err != nil {
		t.Fatalf("write journal: %v", err)
	}

	testlog := strings.Join([]string{
		"=== RUN   TestSingleNode/Phase5_LaunchInstance",
		"    ec2helpers.go:164: describe-vpcs: InvalidParameterValue",
		"--- FAIL: TestSingleNode/Phase5_LaunchInstance (2.50s)",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "test-single.log"), []byte(testlog), 0o644); err != nil {
		t.Fatalf("write test log: %v", err)
	}

	// Per-test artifact subdir (matches harness.ArtifactDir convention).
	perTest := filepath.Join(dir, "TestSingleNode_Phase5_LaunchInstance")
	if err := os.MkdirAll(perTest, 0o755); err != nil {
		t.Fatalf("mkdir per-test: %v", err)
	}
	if err := os.WriteFile(filepath.Join(perTest, "qemu-console.log"), []byte("[qemu] boot\n"), 0o644); err != nil {
		t.Fatalf("write per-test file: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "junit-single.xml"))
	sr, err := ParseFile("junit-single.xml", data)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	rep := Report{Title: "E2E failure analysis", Suites: []SuiteReport{sr}}
	WriteBundles(&rep, dir)

	if rep.Suites[0].Root == nil {
		t.Fatal("no root")
	}
	bp := rep.Suites[0].Root.BundlePath
	if bp == "" {
		t.Fatal("BundlePath not set")
	}
	bundle := filepath.Join(dir, bp)
	for _, want := range []string{"failure.txt", "journal.log", "test.log", "qemu-console.log"} {
		if _, err := os.Stat(filepath.Join(bundle, want)); err != nil {
			t.Errorf("missing %s in bundle %s: %v", want, bp, err)
		}
	}

	jl, _ := os.ReadFile(filepath.Join(bundle, "journal.log"))
	if !strings.Contains(string(jl), "describe-vpcs filter=is_default") {
		t.Errorf("journal.log lacks in-window line:\n%s", jl)
	}
	if strings.Contains(string(jl), "too-early") || strings.Contains(string(jl), "too-late") {
		t.Errorf("journal.log includes out-of-window content:\n%s", jl)
	}

	tl, _ := os.ReadFile(filepath.Join(bundle, "test.log"))
	if !strings.Contains(string(tl), "--- FAIL: TestSingleNode/Phase5_LaunchInstance") {
		t.Errorf("test.log missing FAIL marker:\n%s", tl)
	}

	out := Render(rep)
	wantLink := fmt.Sprintf("- Bundle: `%s/`", bp)
	if !strings.Contains(out, wantLink) {
		t.Errorf("render missing bundle link %q:\n%s", wantLink, out)
	}
}
