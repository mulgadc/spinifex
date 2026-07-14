package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/cmd/ecs-agent/credentials"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
)

// fixedCredsProvider is a static credentials.CredentialsProvider for tests
// that only need a valid SigV4 signature, not real IMDS rotation.
type fixedCredsProvider struct{}

func (fixedCredsProvider) Retrieve(context.Context) (credentials.Credentials, error) {
	return credentials.Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"}, nil
}

func newTestGatewayControlPlane(t *testing.T, srv *httptest.Server) *gatewayControlPlane {
	t.Helper()
	cp, err := newGatewayControlPlane(config{GatewayURL: srv.URL, Region: "us-east-1"}, fixedCredsProvider{})
	if err != nil {
		t.Fatalf("newGatewayControlPlane: %v", err)
	}
	return cp
}

// Register carries the discovered GPU UUIDs as a STRINGSET resource, matching
// the control plane's RegisterContainerInstance GPU parsing (Epic C3).
func TestGatewayControlPlane_Register_SendsGPUStringSet(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cp := newTestGatewayControlPlane(t, srv)
	id := identity{
		ClusterName: "default", InstanceID: "i-1",
		Capacity: detectCapacity([]string{"GPU-aaa", "GPU-bbb"}),
	}
	if err := cp.Register(id); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var in ecs.RegisterContainerInstanceInput
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	var gpu *ecs.Resource
	for _, r := range in.TotalResources {
		if r.Name != nil && *r.Name == "GPU" {
			gpu = r
		}
	}
	if gpu == nil {
		t.Fatal("no GPU resource in TotalResources")
	}
	if *gpu.Type != "STRINGSET" {
		t.Errorf("GPU resource type = %q, want STRINGSET", *gpu.Type)
	}
	got := make([]string, len(gpu.StringSetValue))
	for i, v := range gpu.StringSetValue {
		got[i] = *v
	}
	if len(got) != 2 || got[0] != "GPU-aaa" || got[1] != "GPU-bbb" {
		t.Errorf("GPU UUIDs = %v, want [GPU-aaa GPU-bbb]", got)
	}
}

// A non-GPU host (no discovered UUIDs) registers with no GPU resource at all.
func TestGatewayControlPlane_Register_NoGPUOmitsResource(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cp := newTestGatewayControlPlane(t, srv)
	id := identity{ClusterName: "default", InstanceID: "i-1", Capacity: detectCapacity(nil)}
	if err := cp.Register(id); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var in ecs.RegisterContainerInstanceInput
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, r := range in.TotalResources {
		if r.Name != nil && *r.Name == "GPU" {
			t.Fatalf("unexpected GPU resource on a non-GPU host: %+v", r)
		}
	}
}

// ReportTaskGPU posts the per-container device UUIDs under the ReportTaskGPU
// action, decodable as the handlers_ecs internal shape.
func TestGatewayControlPlane_ReportTaskGPU(t *testing.T) {
	var gotTarget string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTarget = r.Header.Get("X-Amz-Target")
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cp := newTestGatewayControlPlane(t, srv)
	err := cp.ReportTaskGPU("web", "t-1", []handlers_ecs.ContainerGPUReport{
		{Name: "trainer", GPUIDs: []string{"GPU-aaa"}},
	})
	if err != nil {
		t.Fatalf("ReportTaskGPU: %v", err)
	}
	if gotTarget == "" {
		t.Fatal("no X-Amz-Target header sent")
	}

	var in handlers_ecs.ReportTaskGPUInput
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if in.Cluster != "web" || in.Task != "t-1" {
		t.Errorf("cluster/task = %q/%q, want web/t-1", in.Cluster, in.Task)
	}
	if len(in.Containers) != 1 || in.Containers[0].Name != "trainer" || len(in.Containers[0].GPUIDs) != 1 {
		t.Errorf("containers = %+v, want one trainer entry with one UUID", in.Containers)
	}
}
