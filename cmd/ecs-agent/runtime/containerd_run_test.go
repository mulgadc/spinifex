package runtime

import (
	"reflect"
	"testing"
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
