package awsmodel_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
)

func TestRenderCoverageJSONIsDeterministicAndPreservesDispatchStates(t *testing.T) {
	sts, err := awsmodel.CompareOperations(awsmodel.STS, awsmodel.DispatchInventory{
		Registered:  []string{"AssumeRole", "GetCallerIdentity", "GetSessionToken"},
		Stubbed:     []string{"GetSessionToken"},
		Unsupported: []string{"GetCallerIdentity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	iam, err := awsmodel.CompareOperations(awsmodel.IAM, awsmodel.DispatchInventory{})
	if err != nil {
		t.Fatal(err)
	}

	contents, err := awsmodel.RenderCoverageJSON([]awsmodel.OperationCoverage{sts, iam})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		SchemaVersion int `json:"schema_version"`
		ModelSource   struct {
			AWSSDKGoVersion string `json:"aws_sdk_go_version"`
		} `json:"model_source"`
		Services []struct {
			Service    string `json:"service"`
			Operations struct {
				Implemented []string `json:"implemented"`
				Stubbed     []string `json:"stubbed"`
				Unsupported []string `json:"unsupported"`
				Registered  []string `json:"registered"`
			} `json:"operations"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(contents), &document); err != nil {
		t.Fatalf("generated JSON is invalid: %v\n%s", err, contents)
	}
	if document.SchemaVersion != awsmodel.CoverageJSONSchemaVersion {
		t.Errorf("schema version = %d, want %d", document.SchemaVersion, awsmodel.CoverageJSONSchemaVersion)
	}
	if document.ModelSource.AWSSDKGoVersion != awsmodel.SourceSDKVersion {
		t.Errorf("source version = %q, want %q", document.ModelSource.AWSSDKGoVersion, awsmodel.SourceSDKVersion)
	}
	if !strings.Contains(contents, `"registered": []`) {
		t.Errorf("an empty operation set must be an array, not null:\n%s", contents)
	}
	if len(document.Services) != 2 || document.Services[0].Service != "iam" || document.Services[1].Service != "sts" {
		t.Fatalf("services are not deterministic: %#v", document.Services)
	}
	states := document.Services[1].Operations
	if got, want := strings.Join(states.Implemented, ","), "AssumeRole"; got != want {
		t.Errorf("implemented = %q, want %q", got, want)
	}
	if got, want := strings.Join(states.Stubbed, ","), "GetSessionToken"; got != want {
		t.Errorf("stubbed = %q, want %q", got, want)
	}
	if got, want := strings.Join(states.Unsupported, ","), "GetCallerIdentity"; got != want {
		t.Errorf("unsupported = %q, want %q", got, want)
	}
	if got, want := strings.Join(states.Registered, ","), "AssumeRole,GetCallerIdentity,GetSessionToken"; got != want {
		t.Errorf("registered = %q, want %q", got, want)
	}
}

func TestWriteCoverageJSONWritesTheSameDocument(t *testing.T) {
	coverage, err := awsmodel.CompareOperations(awsmodel.STS, awsmodel.DispatchInventory{Registered: []string{"AssumeRole"}})
	if err != nil {
		t.Fatal(err)
	}
	want, err := awsmodel.RenderCoverageJSON([]awsmodel.OperationCoverage{coverage})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "coverage.json")
	if err := awsmodel.WriteCoverageJSON(path, []awsmodel.OperationCoverage{coverage}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("written JSON differs from renderer:\n got: %s\nwant: %s", got, want)
	}
}
