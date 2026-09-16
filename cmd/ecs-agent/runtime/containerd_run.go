package runtime

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cdi"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// cdiVendorGPU is the NVIDIA CDI vendor/class prefix for whole-GPU devices, per
// the NVIDIA CDI naming convention (nvidia.com/gpu=<uuid>).
const cdiVendorGPU = "nvidia.com/gpu"

// cdiDeviceNames maps a container's pinned GPU device UUIDs to CDI device
// names. Empty input (non-GPU container) returns nil so no CDI opt is added.
func cdiDeviceNames(uuids []string) []string {
	if len(uuids) == 0 {
		return nil
	}
	names := make([]string, len(uuids))
	for i, id := range uuids {
		names[i] = cdiVendorGPU + "=" + id
	}
	return names
}

// cdiSpecOpts returns the oci.SpecOpts needed to inject gpuIDs (via
// containerd's CDI machinery) as devices into a container's OCI spec. It
// returns nil for a non-GPU container so Run adds nothing to the opts list.
func cdiSpecOpts(gpuIDs []string) []oci.SpecOpts {
	devices := cdiDeviceNames(gpuIDs)
	if len(devices) == 0 {
		return nil
	}
	return []oci.SpecOpts{cdi.WithCDIDevices(devices...)}
}

// ociCapName renders an ECS Linux capability (Docker's bare name, e.g.
// "SYS_ADMIN") as the OCI runtime-spec name ("CAP_SYS_ADMIN") capabilities.Add/
// Drop expect.
func ociCapName(name string) string {
	if strings.HasPrefix(name, "CAP_") {
		return name
	}
	return "CAP_" + name
}

// ociCapNames maps a list of ECS capability names to their OCI form.
func ociCapNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = ociCapName(n)
	}
	return out
}

// withSysctls sets Linux.Sysctl on the OCI spec from systemControls namespace/
// value pairs. containerd has no built-in SpecOpts for sysctls.
func withSysctls(ctls []SystemControl) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if len(ctls) == 0 {
			return nil
		}
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		if s.Linux.Sysctl == nil {
			s.Linux.Sysctl = map[string]string{}
		}
		for _, c := range ctls {
			s.Linux.Sysctl[c.Namespace] = c.Value
		}
		return nil
	}
}

// blockingStdin is an io.Reader that never returns, backing an interactive
// container's stdin: reads block instead of hitting an immediate EOF, which is
// the "stdin held open" behaviour interactive=true asks for. Nothing in this
// agent attaches to a running container's stdin, so one reader shared by every
// interactive container is enough — it never produces data for anyone to
// demux.
type blockingStdin struct{}

func (blockingStdin) Read([]byte) (int, error) {
	select {}
}

// containerIOCreator returns cio.NullIO for a non-interactive container
// (stdin reads EOF immediately, matching today's behaviour) or a creator that
// keeps stdin open via blockingStdin when interactive is requested.
func containerIOCreator(interactive bool) cio.Creator {
	if !interactive {
		return cio.NullIO
	}
	return cio.NewCreator(cio.WithStreams(blockingStdin{}, io.Discard, io.Discard))
}

var _ Runner = (*containerdPuller)(nil)

// Run creates a container from an already-pulled image and starts its task. The
// container joins the host network namespace (bridge/CNI task networking is a
// later sprint) and carries spec.Labels for the reboot reconciler. id must be
// unique within the "ecs" namespace.
func (p *containerdPuller) Run(ctx context.Context, id string, spec RunSpec) (string, error) {
	ctx = namespaces.WithNamespace(ctx, ecsNamespace)

	ref, err := normalizeRef(spec.Image)
	if err != nil {
		return "", err
	}
	image, err := p.client.GetImage(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("get image %s: %w", ref, err)
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithHostHostsFile,
		oci.WithHostResolvconf,
	}
	if spec.NetnsPath != "" {
		// awsvpc: join the task ENI netns built by the agent.
		specOpts = append(specOpts, oci.WithLinuxNamespace(specs.LinuxNamespace{
			Type: specs.NetworkNamespace,
			Path: spec.NetnsPath,
		}))
	} else {
		// bridge/host: share the VM (host) netns.
		specOpts = append(specOpts, oci.WithHostNamespace(specs.NetworkNamespace))
	}
	if len(spec.Command) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(spec.Command...))
	}
	if len(spec.Env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(envSlice(spec.Env)))
	}
	specOpts = append(specOpts, cdiSpecOpts(spec.GPUIDs)...)
	if spec.User != "" {
		specOpts = append(specOpts, oci.WithUser(spec.User))
	}
	if spec.ReadonlyRootFilesystem != nil && *spec.ReadonlyRootFilesystem {
		specOpts = append(specOpts, oci.WithRootFSReadonly())
	}
	if spec.Privileged != nil && *spec.Privileged {
		specOpts = append(specOpts, oci.WithPrivileged)
	}
	if spec.PseudoTerminal != nil && *spec.PseudoTerminal {
		specOpts = append(specOpts, oci.WithTTY)
	}
	specOpts = append(specOpts, withSysctls(spec.SystemControls))
	if len(spec.CapAdd) > 0 {
		specOpts = append(specOpts, oci.WithAddedCapabilities(ociCapNames(spec.CapAdd)))
	}
	if len(spec.CapDrop) > 0 {
		specOpts = append(specOpts, oci.WithDroppedCapabilities(ociCapNames(spec.CapDrop)))
	}

	container, err := p.client.NewContainer(ctx, id,
		containerd.WithNewSnapshot(id+"-snapshot", image),
		containerd.WithNewSpec(specOpts...),
		containerd.WithContainerLabels(spec.Labels),
	)
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", id, err)
	}

	interactive := spec.Interactive != nil && *spec.Interactive
	task, err := container.NewTask(ctx, containerIOCreator(interactive))
	if err != nil {
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return "", fmt.Errorf("create task %s: %w", id, err)
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(ctx)
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return "", fmt.Errorf("start task %s: %w", id, err)
	}
	return id, nil
}

// Wait blocks until the container's task exits and returns its exit code.
func (p *containerdPuller) Wait(ctx context.Context, containerID string) (RunStatus, error) {
	ctx = namespaces.WithNamespace(ctx, ecsNamespace)
	container, err := p.client.LoadContainer(ctx, containerID)
	if err != nil {
		return RunStatus{}, fmt.Errorf("load container %s: %w", containerID, err)
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return RunStatus{}, fmt.Errorf("attach task %s: %w", containerID, err)
	}
	statusC, err := task.Wait(ctx)
	if err != nil {
		return RunStatus{}, fmt.Errorf("wait task %s: %w", containerID, err)
	}
	status := <-statusC
	code, _, err := status.Result()
	return RunStatus{ExitCode: int(code)}, err
}

// List enumerates every container in the ecs namespace with its labels and
// whether its task is running, so the agent can re-adopt live containers after a
// restart. Per-container label/task errors are treated as "not running" rather
// than failing the whole list.
func (p *containerdPuller) List(ctx context.Context) ([]Container, error) {
	ctx = namespaces.WithNamespace(ctx, ecsNamespace)
	containers, err := p.client.Containers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]Container, 0, len(containers))
	for _, c := range containers {
		labels, _ := c.Labels(ctx)
		running := false
		if task, terr := c.Task(ctx, nil); terr == nil {
			if status, serr := task.Status(ctx); serr == nil {
				running = status.Status == containerd.Running
			}
		}
		out = append(out, Container{ID: c.ID(), Labels: labels, Running: running})
	}
	return out, nil
}

// Stop enforces a container's stopTimeout: SIGTERM the task, wait up to
// timeout for it to exit on its own, then fall through to Remove's forced
// kill+delete regardless of whether it exited in time. A container with no
// live task (already gone) or that fails to accept the signal goes straight to
// Remove, matching Remove's own already-gone tolerance.
func (p *containerdPuller) Stop(ctx context.Context, containerID string, timeout time.Duration) error {
	ctx = namespaces.WithNamespace(ctx, ecsNamespace)
	container, err := p.client.LoadContainer(ctx, containerID)
	if err != nil {
		return nil //nolint:nilerr // load failure means the container is already gone
	}
	task, terr := container.Task(ctx, nil)
	if terr != nil {
		return p.Remove(ctx, containerID)
	}
	statusC, werr := task.Wait(ctx)
	if werr == nil && task.Kill(ctx, syscall.SIGTERM) == nil {
		select {
		case <-statusC:
		case <-time.After(timeout):
		}
	}
	return p.Remove(ctx, containerID)
}

// Remove kills and deletes the container's task, then the container + snapshot.
func (p *containerdPuller) Remove(ctx context.Context, containerID string) error {
	ctx = namespaces.WithNamespace(ctx, ecsNamespace)
	container, err := p.client.LoadContainer(ctx, containerID)
	if err != nil {
		return nil //nolint:nilerr // load failure means the container is already gone
	}
	if task, terr := container.Task(ctx, nil); terr == nil {
		_, _ = task.Delete(ctx, containerd.WithProcessKill)
	}
	return container.Delete(ctx, containerd.WithSnapshotCleanup)
}

// envSlice renders an env map as sorted "K=V" entries for deterministic specs.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
