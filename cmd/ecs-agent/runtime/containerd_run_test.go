package runtime

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/cio"
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

// An interactive container's stdin read blocks until the container is released,
// then reports EOF so containerd's copier goroutine exits instead of parking
// for the life of the agent.
func TestHeldStdin_ReadsEOFOnceReleased(t *testing.T) {
	p := &containerdPuller{}
	p.containerIOCreator("c1", true, false)

	p.mu.Lock()
	done := p.stdinDone["c1"]
	p.mu.Unlock()
	if done == nil {
		t.Fatal("interactive container registered no stdin channel")
	}

	read := make(chan error, 1)
	go func() {
		_, err := heldStdin{done: done}.Read(make([]byte, 1))
		read <- err
	}()

	select {
	case err := <-read:
		t.Fatalf("read returned before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	p.releaseStdin("c1")

	select {
	case err := <-read:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("want io.EOF after release, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not return after release")
	}
}

// A non-interactive container uses cio.NullIO and registers nothing to release.
func TestContainerIOCreator_NonInteractiveRegistersNothing(t *testing.T) {
	p := &containerdPuller{}
	p.containerIOCreator("c1", false, false)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.stdinDone) != 0 {
		t.Fatalf("want no registered stdin channels, got %d", len(p.stdinDone))
	}
}

// applyIOOpts resolves the cio options onto a Streams the way cio.NewCreator
// does, which is the only way to observe what the creator was built with.
func applyIOOpts(opts []cio.Opt) cio.Streams {
	var s cio.Streams
	for _, o := range opts {
		o(&s)
	}
	return s
}

// A terminal container needs the terminal flag set on its FIFO set, else the
// shim creates no console socket and runc refuses to start the container at all.
// The terminal carries no separate stderr stream.
func TestIOStreamOpts_TerminalRequestsAConsole(t *testing.T) {
	s := applyIOOpts(ioStreamOpts(nil, true))
	if !s.Terminal {
		t.Error("terminal container: want Terminal set")
	}
	if s.Stderr != nil {
		t.Errorf("terminal container: want no stderr stream, got %#v", s.Stderr)
	}
	if s.Stdout == nil {
		t.Error("terminal container: want a stdout stream")
	}
}

// A container with stdin but no terminal keeps both output streams and asks for
// no console.
func TestIOStreamOpts_NonTerminalKeepsStderr(t *testing.T) {
	s := applyIOOpts(ioStreamOpts(heldStdin{}, false))
	if s.Terminal {
		t.Error("non-terminal container: want Terminal unset")
	}
	if s.Stderr == nil {
		t.Error("non-terminal container: want a stderr stream")
	}
	if s.Stdin == nil {
		t.Error("non-terminal interactive container: want a stdin stream")
	}
}

// A terminal container that is not interactive still gets a creator rather than
// cio.NullIO, and registers no stdin to release.
func TestContainerIOCreator_TerminalWithoutInteractive(t *testing.T) {
	p := &containerdPuller{}
	if c := p.containerIOCreator("c1", false, true); c == nil {
		t.Fatal("terminal container: want a creator")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.stdinDone) != 0 {
		t.Fatalf("want no registered stdin channels, got %d", len(p.stdinDone))
	}
}

// Remove calls releaseStdin unconditionally and Stop falls through to Remove,
// so an unknown container and a second release must both be no-ops.
func TestReleaseStdin_UnknownAndRepeatedAreSafe(t *testing.T) {
	p := &containerdPuller{}
	p.releaseStdin("never-registered")

	p.containerIOCreator("c1", true, false)
	p.releaseStdin("c1")
	p.releaseStdin("c1")
}
