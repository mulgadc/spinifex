package awsmodel_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/mulgadc/spinifex/internal/awsmodel"
)

// stagePages copies the checked-in metadata into a temp directory so a write
// test cannot touch docs/coverage.
func stagePages(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	contents, err := os.ReadFile(filepath.Join("..", "..", "docs", "coverage", "pages.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pages.json"), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func stsCoverage(t *testing.T) OperationCoverage {
	t.Helper()
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}
	return coverage
}

func TestWritePagesWritesAServicePageAndTheIndex(t *testing.T) {
	t.Parallel()
	dir := stagePages(t)

	if err := WritePages(dir, []OperationCoverage{stsCoverage(t)}); err != nil {
		t.Fatal(err)
	}

	service, err := os.ReadFile(filepath.Join(dir, "sts", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(service), "| `AssumeRole` |") {
		t.Errorf("service page is missing its operation table:\n%s", service)
	}
	index, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "[STS](/coverage/sts)") {
		t.Errorf("index does not link the service page:\n%s", index)
	}
}

// The gaps the pages leave out are still written down, in a file the docs site
// is not configured to render.
func TestWritePagesWritesTheInternalReport(t *testing.T) {
	t.Parallel()
	dir := stagePages(t)
	coverage := stsCoverage(t)

	if err := WritePages(dir, []OperationCoverage{coverage}); err != nil {
		t.Fatal(err)
	}

	report, err := os.ReadFile(filepath.Join(dir, "internal-report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range coverage.Missing {
		if !strings.Contains(string(report), "- `"+operation+"`") {
			t.Errorf("internal report omits unimplemented operation %q:\n%s", operation, report)
		}
	}
	if !strings.Contains(string(report), "% ") && !strings.Contains(string(report), "%)") {
		t.Errorf("internal report carries no coverage figure:\n%s", report)
	}
}

func TestWritePagesIncludesAnIntroAndOverwritesTheGeneratedPage(t *testing.T) {
	t.Parallel()
	dir := stagePages(t)
	pageDir := filepath.Join(dir, "sts")
	if err := os.MkdirAll(pageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pageDir, "intro.md"), []byte("### Scope\n\nCarried prose.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pageDir, "README.md"), []byte("stale hand edit"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WritePages(dir, []OperationCoverage{stsCoverage(t)}); err != nil {
		t.Fatal(err)
	}

	page, err := os.ReadFile(filepath.Join(pageDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "stale hand edit") {
		t.Error("a hand edit to a generated page survived regeneration")
	}
	if !strings.Contains(string(page), "### Scope\n\nCarried prose.") {
		t.Errorf("page does not carry its intro:\n%s", page)
	}
}

// A service the page set does not describe is skipped rather than crashing the
// run or publishing a page with no metadata on it.
func TestWritePagesSkipsAServiceWithNoMetadata(t *testing.T) {
	t.Parallel()
	dir := stageWithout(t, S3)
	coverage, err := CompareOperations(S3, DispatchInventory{Registered: []string{"ListBuckets"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := WritePages(dir, []OperationCoverage{coverage}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "s3")); !os.IsNotExist(err) {
		t.Errorf("a page was written for a service with no metadata: %v", err)
	}
}

// stageWithout stages the checked-in metadata with one service dropped, for the
// cases about a service the page set does not describe.
func stageWithout(t *testing.T, service Service) string {
	t.Helper()
	dir := stagePages(t)
	path := filepath.Join(dir, "pages.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var pages map[string]any
	if err := json.Unmarshal(contents, &pages); err != nil {
		t.Fatal(err)
	}
	services, ok := pages["services"].(map[string]any)
	if !ok {
		t.Fatalf("staged metadata has no services: %s", contents)
	}
	delete(services, string(service))

	reduced, err := json.Marshal(pages)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, reduced, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWritePagesReportsMissingMetadata(t *testing.T) {
	t.Parallel()
	if err := WritePages(t.TempDir(), nil); err == nil {
		t.Fatal("WritePages accepted a directory with no pages.json")
	}
}
