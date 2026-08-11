package awsmodel

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestServices(t *testing.T) {
	want := []Service{ACM, EC2, ECR, ECS, ElasticLoadBalancingV2, IAM, RDS, S3, STS}
	if got := Services(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Services() = %v, want %v", got, want)
	}
}

func TestLoadAllModels(t *testing.T) {
	wantSizes := map[Service]struct {
		apiVersion string
		operations int
		shapes     int
	}{
		ACM:                    {"2015-12-08", 15, 102},
		EC2:                    {"2016-11-15", 625, 3198},
		ECR:                    {"2015-09-21", 47, 330},
		ECS:                    {"2014-11-13", 56, 400},
		ElasticLoadBalancingV2: {"2015-12-01", 46, 357},
		IAM:                    {"2010-05-08", 159, 497},
		RDS:                    {"2014-10-31", 162, 745},
		S3:                     {"2006-03-01", 99, 604},
		STS:                    {"2011-06-15", 8, 76},
	}

	for service, want := range wantSizes {
		t.Run(string(service), func(t *testing.T) {
			model, err := Load(service)
			if err != nil {
				t.Fatalf("Load(%q): %v", service, err)
			}
			if model.Service() != service {
				t.Errorf("Service() = %q, want %q", model.Service(), service)
			}
			if model.Metadata().APIVersion != want.apiVersion || model.Metadata().Protocol == "" {
				t.Errorf("metadata is incomplete: %+v", model.Metadata())
			}
			if got := len(model.Operations()); got != want.operations {
				t.Errorf("operation count = %d, want %d", got, want.operations)
			}
			if got := len(model.Shapes()); got != want.shapes {
				t.Errorf("shape count = %d, want %d", got, want.shapes)
			}
		})
	}
}

func TestLoadEC2IndexesOperationsAndShapes(t *testing.T) {
	model, err := Load(EC2)
	if err != nil {
		t.Fatal(err)
	}

	operation, ok := model.Operation("DescribeInstances")
	if !ok {
		t.Fatal("DescribeInstances operation was not indexed")
	}
	if operation.Output == nil || operation.Output.Shape != "DescribeInstancesResult" {
		t.Fatalf("DescribeInstances output = %+v, want DescribeInstancesResult", operation.Output)
	}

	shape, ok := model.Shape(operation.Output.Shape)
	if !ok {
		t.Fatalf("output shape %q was not indexed", operation.Output.Shape)
	}
	if reservationSet, ok := shape.Members["Reservations"]; !ok || reservationSet.Shape != "ReservationList" {
		t.Errorf("Reservations member = %+v, want ReservationList", reservationSet)
	}
}

func TestLoadPreservesWrappersAndErrorMetadata(t *testing.T) {
	model, err := Load(IAM)
	if err != nil {
		t.Fatal(err)
	}

	getUser, ok := model.Operation("GetUser")
	if !ok || getUser.Output == nil || getUser.Output.ResultWrapper != "GetUserResult" {
		t.Fatalf("GetUser output = %+v, want GetUserResult wrapper", getUser)
	}

	createUser, ok := model.Operation("CreateUser")
	if !ok || len(createUser.Errors) == 0 {
		t.Fatalf("CreateUser operation has no modelled errors: %+v", createUser)
	}
	errorShape, ok := model.Shape(createUser.Errors[0].Shape)
	if !ok || errorShape.Error == nil || errorShape.Error.Code == "" || errorShape.Error.HTTPStatusCode == 0 {
		t.Fatalf("CreateUser error metadata was not loaded: %+v", errorShape)
	}
}

func TestLoadRejectsUnknownService(t *testing.T) {
	_, err := Load("not-a-service")
	if err == nil || !strings.Contains(err.Error(), `unsupported service "not-a-service"`) {
		t.Fatalf("Load(unknown) error = %v", err)
	}
}

func TestLoadCachesModelAcrossConcurrentCalls(t *testing.T) {
	const callers = 16
	models := make(chan *Model, callers)
	errors := make(chan error, callers)

	var wait sync.WaitGroup
	for range callers {
		wait.Go(func() {
			model, err := Load(STS)
			models <- model
			errors <- err
		})
	}
	wait.Wait()
	close(models)
	close(errors)

	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *Model
	for model := range models {
		if first == nil {
			first = model
			continue
		}
		if model != first {
			t.Fatal("Load returned different cached model pointers")
		}
	}
}
