package awsmodel

import (
	"strings"
	"testing"
)

func TestCompareOperations(t *testing.T) {
	coverage, err := CompareOperations(STS, DispatchInventory{
		Registered:  []string{"AssumeRole", "ExtraOperation", "GetCallerIdentity", "GetSessionToken"},
		Stubbed:     []string{"GetSessionToken"},
		Unsupported: []string{"GetCallerIdentity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(coverage.Implemented) != 1 || coverage.Implemented[0] != "AssumeRole" {
		t.Fatalf("implemented = %v, want [AssumeRole]", coverage.Implemented)
	}
	if len(coverage.Missing) != 5 {
		t.Fatalf("missing count = %d, want 5: %v", len(coverage.Missing), coverage.Missing)
	}
	if len(coverage.Extra) != 1 || coverage.Extra[0] != "ExtraOperation" {
		t.Fatalf("extra = %v, want [ExtraOperation]", coverage.Extra)
	}
}

func TestCompareOperationsRejectsInvalidInventory(t *testing.T) {
	_, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}, Stubbed: []string{"Missing"}})
	if err == nil || !strings.Contains(err.Error(), `stubbed operation "Missing" is not registered`) {
		t.Fatalf("error = %v", err)
	}
}

func TestRenderCoverageMarkdown(t *testing.T) {
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}
	report := RenderCoverageMarkdown([]OperationCoverage{coverage})
	for _, want := range []string{
		"# AWS model operation coverage",
		"| sts | 2011-06-15 | 8 | 1 | 1 | 0 | 0 | 7 | 0 |",
		"Implements **1 of 8** modelled operations (12.5%).",
		"<summary>Missing from dispatch (7)</summary>",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not contain %q:\n%s", want, report)
		}
	}
}
