package runtime

import (
	"context"
	"reflect"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// cdiDeviceNames maps a container's pinned GPU UUIDs to CDI device names
// (nvidia.com/gpu=<uuid>); Run wires the result into oci.WithCDIDevices when
// non-empty, so this is the injection logic's unit-testable core.
func TestCDIDeviceNames(t *testing.T) {
	cases := []struct {
		name  string
		uuids []string
		want  []string
	}{
		{name: "empty", uuids: nil, want: nil},
		{name: "single", uuids: []string{"GPU-aaaaaaaa-1111-2222-3333-444444444444"},
			want: []string{"nvidia.com/gpu=GPU-aaaaaaaa-1111-2222-3333-444444444444"}},
		{name: "multiple preserves order", uuids: []string{"GPU-aaa", "GPU-bbb"},
			want: []string{"nvidia.com/gpu=GPU-aaa", "nvidia.com/gpu=GPU-bbb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cdiDeviceNames(tc.uuids)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cdiDeviceNames(%v) = %v, want %v", tc.uuids, got, tc.want)
			}
		})
	}
}

// TestCDISpecOpts_Wiring confirms Run's specOpts list picks up exactly one CDI
// opt when GPUIDs is non-empty, and none for a non-GPU container — the wiring
// Run relies on (a live oci.Spec is needed to unit test the opt's effect).
func TestCDISpecOpts_Wiring(t *testing.T) {
	if opts := cdiSpecOpts(nil); len(opts) != 0 {
		t.Errorf("non-GPU container: want no spec opts, got %d", len(opts))
	}
	opts := cdiSpecOpts([]string{"GPU-aaa", "GPU-bbb"})
	if len(opts) != 1 {
		t.Fatalf("GPU container: want exactly one CDI spec opt, got %d", len(opts))
	}
	if opts[0] == nil {
		t.Error("GPU container: spec opt is nil")
	}
}

// TestOCICapNames verifies ECS's bare capability names (Docker convention,
// e.g. "SYS_PTRACE") are prefixed to the OCI runtime-spec form the process
// capability sets expect, and a name already carrying the prefix passes
// through unchanged.
func TestOCICapNames(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "empty", in: nil, want: nil},
		{name: "bare", in: []string{"SYS_PTRACE"}, want: []string{"CAP_SYS_PTRACE"}},
		{name: "already prefixed", in: []string{"CAP_NET_ADMIN"}, want: []string{"CAP_NET_ADMIN"}},
		{name: "mixed", in: []string{"SYS_PTRACE", "CAP_NET_ADMIN"}, want: []string{"CAP_SYS_PTRACE", "CAP_NET_ADMIN"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ociCapNames(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ociCapNames(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestWithSysctls verifies systemControls namespace/value pairs land on
// Linux.Sysctl, and an empty list leaves the spec's Linux section untouched
// rather than allocating an empty map (the "requests nothing" case).
func TestWithSysctls(t *testing.T) {
	s := &specs.Spec{}
	if err := withSysctls(nil)(context.Background(), nil, nil, s); err != nil {
		t.Fatalf("empty ctls: %v", err)
	}
	if s.Linux != nil {
		t.Errorf("empty ctls: want no Linux section, got %+v", s.Linux)
	}

	ctls := []SystemControl{{Namespace: "net.core.somaxconn", Value: "1024"}}
	if err := withSysctls(ctls)(context.Background(), nil, nil, s); err != nil {
		t.Fatalf("with ctls: %v", err)
	}
	if s.Linux == nil || s.Linux.Sysctl["net.core.somaxconn"] != "1024" {
		t.Errorf("want sysctl net.core.somaxconn=1024, got %+v", s.Linux)
	}
}
