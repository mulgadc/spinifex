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
	if len(coverage.Missing) != 8 {
		t.Fatalf("missing count = %d, want 8: %v", len(coverage.Missing), coverage.Missing)
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

func TestRenderCoverageSummary(t *testing.T) {
	coverage, err := CompareOperations(STS, DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}
	report := RenderCoverageSummary([]OperationCoverage{coverage})
	for _, want := range []string{
		ModelSourceDescription(),
		"1 of  11 implemented (  9.1%)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("summary does not contain %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "<details>") {
		t.Errorf("summary contains raw HTML the docs site would escape:\n%s", report)
	}
}
