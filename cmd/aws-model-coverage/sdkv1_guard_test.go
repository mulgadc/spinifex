package main

import (
	"fmt"
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/mulgadc/spinifex/internal/awsmodel/sdkv1"
)

type serviceOperation struct {
	service   awsmodel.Service
	operation string
}

// stubsKeptWithoutSDKV1 lists the operations registered as a stub or a refusal
// although aws-sdk-go v1 cannot call them. Each one returns an error and never
// a fake success. Do not add to it: delete the registration instead.
var stubsKeptWithoutSDKV1 = map[serviceOperation]string{
	{awsmodel.ECR, "ListRepositories"}:              "returns NotImplemented (501); registered before the SDK v1 check existed",
	{awsmodel.ECR, "ReplicateImage"}:                "returns NotImplemented (501); registered before the SDK v1 check existed",
	{awsmodel.ECR, "GetImageScanningConfiguration"}: "returns OperationNotSupportedException because Spinifex runs no scanner; registered before the SDK v1 check existed",
}

// noSDKV1Violations returns the stubbed or refused operations absent from
// aws-sdk-go v1 and not allowlisted, and the allowlist entries that no longer
// apply. A real handler is not checked: only a no-op registration is the
// problem this guards.
func noSDKV1Violations(coverages []awsmodel.OperationCoverage, allow map[serviceOperation]string) (violations, stale []string, err error) {
	seen := map[serviceOperation]bool{}
	for _, coverage := range coverages {
		if coverage.Opaque {
			continue
		}
		v1, ok := sdkv1.Operations(coverage.Service)
		if !ok {
			return nil, nil, fmt.Errorf("no aws-sdk-go v1 client for %s", coverage.Service)
		}
		for _, operation := range slices.Concat(coverage.Stubbed, coverage.Unsupported) {
			if slices.Contains(v1, operation) {
				continue
			}
			key := serviceOperation{coverage.Service, operation}
			seen[key] = true
			if _, listed := allow[key]; !listed {
				violations = append(violations, fmt.Sprintf("%s %s", coverage.Service, operation))
			}
		}
	}
	for key := range allow {
		if !seen[key] {
			stale = append(stale, fmt.Sprintf("%s %s", key.service, key.operation))
		}
	}
	slices.Sort(stale)
	return violations, stale, nil
}

func TestNoStubsForOperationsAbsentFromSDKV1(t *testing.T) {
	coverages, err := compareAll()
	if err != nil {
		t.Fatal(err)
	}
	violations, stale, err := noSDKV1Violations(coverages, stubsKeptWithoutSDKV1)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Errorf("%s is registered as a stub or refusal but is not in aws-sdk-go v1, so nothing Spinifex is tested with can call it. "+
			"Remove the registration: an unregistered operation already returns the proper unsupported error, and a stub that fakes success is worse. "+
			"A coverage \"missing\" entry is not a task, and an operation in requires_sdk_v2 is out of scope until the SDK v2 migration (internal/awsmodel/models/README.md)", v)
	}
	for _, s := range stale {
		t.Errorf("%s is allowlisted in stubsKeptWithoutSDKV1 but is no longer a stub or refusal absent from aws-sdk-go v1: remove the entry", s)
	}
}

func TestNoSDKV1GuardFlagsAnUnlistedStub(t *testing.T) {
	coverage := awsmodel.OperationCoverage{
		Service: awsmodel.ECR,
		Stubbed: []string{"ListRepositories", "PutImage"},
	}
	violations, stale, err := noSDKV1Violations([]awsmodel.OperationCoverage{coverage}, map[serviceOperation]string{
		{awsmodel.ECR, "Retired"}: "gone",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ecr ListRepositories"}; !slices.Equal(violations, want) {
		t.Errorf("violations = %v, want %v (PutImage is in SDK v1)", violations, want)
	}
	if want := []string{"ecr Retired"}; !slices.Equal(stale, want) {
		t.Errorf("stale = %v, want %v", stale, want)
	}
}
