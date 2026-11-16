package awsmodel_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
)

func TestCompareOperationsSplitsGapsBySDKV1(t *testing.T) {
	model, err := awsmodel.Load(awsmodel.STS)
	if err != nil {
		t.Fatal(err)
	}
	modelled := model.Operations()
	// Only AssumeRole is registered; the first other operation is callable
	// from SDK v1 and the rest are not.
	var inV1, onlyV2 string
	for _, operation := range modelled {
		if operation != "AssumeRole" && inV1 == "" {
			inV1 = operation
		}
	}
	for _, operation := range modelled {
		if operation != "AssumeRole" && operation != inV1 {
			onlyV2 = operation
			break
		}
	}
	coverage, err := awsmodel.CompareOperations(awsmodel.STS, awsmodel.DispatchInventory{
		Registered:      []string{"AssumeRole"},
		SDKV1Operations: []string{"AssumeRole", inV1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(coverage.Missing, inV1) || slices.Contains(coverage.RequiresSDKV2, inV1) {
		t.Errorf("%s is in SDK v1 so must be missing, got missing=%v requires=%v", inV1, coverage.Missing, coverage.RequiresSDKV2)
	}
	if !slices.Contains(coverage.RequiresSDKV2, onlyV2) || slices.Contains(coverage.Missing, onlyV2) {
		t.Errorf("%s is not in SDK v1 so must require SDK v2, got missing=%v requires=%v", onlyV2, coverage.Missing, coverage.RequiresSDKV2)
	}
	if slices.Contains(coverage.RequiresSDKV2, "AssumeRole") {
		t.Error("a registered operation must never be listed as requiring SDK v2")
	}
	if len(coverage.Missing)+len(coverage.RequiresSDKV2) != len(modelled)-1 {
		t.Errorf("gaps = %d + %d, want %d", len(coverage.Missing), len(coverage.RequiresSDKV2), len(modelled)-1)
	}

	unknown, err := awsmodel.CompareOperations(awsmodel.STS, awsmodel.DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.RequiresSDKV2) != 0 {
		t.Errorf("without SDK information nothing may be classed as requiring SDK v2, got %v", unknown.RequiresSDKV2)
	}
}

func TestRequiresSDKV2IsRenderedInJSONAndPages(t *testing.T) {
	coverage := awsmodel.OperationCoverage{
		Service:       awsmodel.STS,
		APIVersion:    "2011-06-15",
		Modelled:      []string{"AssumeRole", "OnlyInV2"},
		Registered:    []string{"AssumeRole"},
		Implemented:   []string{"AssumeRole"},
		RequiresSDKV2: []string{"OnlyInV2"},
	}
	contents, err := awsmodel.RenderCoverageJSON([]awsmodel.OperationCoverage{coverage})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contents, `"requires_sdk_v2": [`+"\n"+`          "OnlyInV2"`) || !strings.Contains(contents, `"missing": []`) {
		t.Errorf("JSON does not carry the split:\n%s", contents)
	}

	if !strings.Contains(contents, `"schema_version": 1,`) {
		t.Errorf("schema_version must stay 1: capability_profiles.py rejects any other version:\n%s", contents)
	}

	pages := testPageSet(t)
	page, err := awsmodel.RenderServicePage(coverage, pages, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "### Requires AWS SDK v2") || !strings.Contains(page, "| `OnlyInV2` |") {
		t.Errorf("service page lacks the SDK v2 section:\n%s", page)
	}
	operations, _, _ := strings.Cut(page, "### Requires AWS SDK v2")
	if strings.Contains(operations, "OnlyInV2") {
		t.Error("an SDK-v2-only operation must not be listed under Operations")
	}
	index, err := awsmodel.RenderIndexPage([]awsmodel.OperationCoverage{coverage}, pages, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(index, "A further **1 operations**") {
		t.Errorf("index lacks the SDK v2 count:\n%s", index)
	}
	report := awsmodel.RenderInternalReport([]awsmodel.OperationCoverage{coverage}, pages)
	if !strings.Contains(report, "### Requires SDK v2") || strings.Contains(report, "### Not implemented") {
		t.Errorf("internal report does not separate the SDK v2 list:\n%s", report)
	}
}

func TestIndexAndReportCountSDKV2PerService(t *testing.T) {
	sts := awsmodel.OperationCoverage{
		Service: awsmodel.STS, APIVersion: "2011-06-15",
		Modelled: []string{"A", "B", "C"}, Registered: []string{"A"}, Implemented: []string{"A"},
		RequiresSDKV2: []string{"B", "C"},
	}
	iam := awsmodel.OperationCoverage{
		Service: awsmodel.IAM, APIVersion: "2010-05-08",
		Modelled: []string{"A", "B"}, Registered: []string{"A"}, Implemented: []string{"A"},
		RequiresSDKV2: []string{"B"},
	}
	coverages := []awsmodel.OperationCoverage{sts, iam}
	pages := testPageSet(t)
	index, err := awsmodel.RenderIndexPage(coverages, pages, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"| [IAM](/coverage/iam) | 1 | 1 |", "| [STS](/coverage/sts) | 1 | 2 |", "| **Total** | **2** | **3** |"} {
		if !strings.Contains(index, row) {
			t.Errorf("index lacks row %q:\n%s", row, index)
		}
	}
	report := awsmodel.RenderInternalReport(coverages, pages)
	if !strings.Contains(report, "0 stubbed, 2 require SDK v2,") || !strings.Contains(report, "0 stubbed, 1 require SDK v2,") {
		t.Errorf("internal report summary lacks per-service SDK v2 counts:\n%s", report)
	}
}
