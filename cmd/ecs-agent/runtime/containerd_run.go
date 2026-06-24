package runtime

import (
	"context"
	"fmt"
	"sort"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

var _ Runner = (*containerdPuller)(nil)

// Run creates a container from an already-pulled image and starts its task. The
// container joins the host network namespace (bridge/CNI task networking is a
// later sprint) and carries spec.Labels for the reboot reconciler. id must be
// unique within the "ecs" namespace.
func (p *containerdPuller) Run(ctx context.Context, id string, spec RunSpec) (string, error) {
	ctx = namespaces.WithNamespace(ctx, ecsNamespace)

	image, err := p.client.GetImage(ctx, spec.Image)
	if err != nil {
		return "", fmt.Errorf("get image %s: %w", spec.Image, err)
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithHostNamespace(specs.NetworkNamespace),
		oci.WithHostHostsFile,
		oci.WithHostResolvconf,
	}
	if len(spec.Command) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(spec.Command...))
	}
	if len(spec.Env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(envSlice(spec.Env)))
	}

	container, err := p.client.NewContainer(ctx, id,
		containerd.WithNewSnapshot(id+"-snapshot", image),
		containerd.WithNewSpec(specOpts...),
		containerd.WithContainerLabels(spec.Labels),
	)
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", id, err)
	}

	task, err := container.NewTask(ctx, cio.NullIO)
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
