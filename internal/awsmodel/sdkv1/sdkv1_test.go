package sdkv1

import (
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
)

func TestOperationsComeFromTheSDKClient(t *testing.T) {
	ec2, ok := Operations(awsmodel.EC2)
	if !ok {
		t.Fatal("no ec2 client")
	}
	if !slices.IsSorted(ec2) {
		t.Error("operations are not sorted")
	}
	if !slices.Contains(ec2, "RunInstances") {
		t.Error("RunInstances is missing")
	}
	if slices.Contains(ec2, "NewRequest") || slices.Contains(ec2, "") {
		t.Error("a non-operation method was counted")
	}
	ecr, _ := Operations(awsmodel.ECR)
	if slices.Contains(ecr, "ListRepositories") {
		t.Error("ListRepositories is not an ECR operation in aws-sdk-go v1")
	}
}

func TestEveryEmbeddedModelHasAnSDKClient(t *testing.T) {
	for _, service := range awsmodel.Services() {
		if operations, ok := Operations(service); !ok || len(operations) == 0 {
			t.Errorf("%s has no aws-sdk-go v1 operations; register its client in sdkv1.clients", service)
		}
	}
}

func TestUnknownServiceReportsNotFound(t *testing.T) {
	if _, ok := Operations(awsmodel.Service("nope")); ok {
		t.Error("unknown service reported as found")
	}
}
