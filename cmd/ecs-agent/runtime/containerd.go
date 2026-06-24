package runtime

import (
	"context"
	"fmt"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// ecsNamespace isolates ecs-agent images/containers in containerd, mirroring the
// "moby"/"k8s.io" convention other runtimes use.
const ecsNamespace = "ecs"

// servingProbeTimeout bounds the boot-time daemon liveness check.
const servingProbeTimeout = 3 * time.Second

// containerdPuller drives a real containerd daemon over its unix socket.
type containerdPuller struct {
	client *containerd.Client
}

var _ ImagePuller = (*containerdPuller)(nil)

// New connects to the containerd daemon at socket (e.g.
// "/run/containerd/containerd.sock") and returns an ImagePuller. The containerd
// client dials lazily, so New actively probes the daemon with IsServing and
// returns an error if it is unreachable — otherwise a dead socket would only
// surface on the first pull, and the agent's boot-time "containerd unavailable"
// log would never fire.
func New(socket string) (Runtime, error) {
	client, err := containerd.New(socket, containerd.WithDefaultNamespace(ecsNamespace))
	if err != nil {
		return nil, fmt.Errorf("connect containerd %s: %w", socket, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), servingProbeTimeout)
	defer cancel()
	serving, err := client.IsServing(ctx)
	if err != nil || !serving {
		_ = client.Close()
		return nil, fmt.Errorf("containerd %s not serving: %w", socket, err)
	}
	return &containerdPuller{client: client}, nil
}

// Pull resolves credentials via r, then pulls and unpacks the image.
func (p *containerdPuller) Pull(ctx context.Context, spec PullSpec, r Resolver) (Image, error) {
	ctx = namespaces.WithNamespace(ctx, ecsNamespace)

	resolver, err := newDockerResolver(ctx, spec.Ref, r)
	if err != nil {
		return Image{}, err
	}

	img, err := p.client.Pull(ctx, spec.Ref,
		containerd.WithPullUnpack,
		containerd.WithResolver(resolver),
	)
	if err != nil {
		return Image{}, fmt.Errorf("pull %s: %w", spec.Ref, err)
	}

	target := img.Target()
	return Image{
		Ref:    spec.Ref,
		Digest: target.Digest.String(),
		Size:   target.Size,
	}, nil
}

// Close releases the containerd client connection.
func (p *containerdPuller) Close() error {
	if p.client == nil {
		return nil
	}
	return p.client.Close()
}

// newDockerResolver builds a containerd remote resolver whose per-host
// credentials come from r (the ECR token resolver). Credentials are fetched once
// for ref and reused for every host containerd asks about during the pull.
func newDockerResolver(ctx context.Context, ref string, r Resolver) (remotes.Resolver, error) {
	creds := func(string) (string, string, error) { return "", "", nil }
	if r != nil {
		user, pass, _, err := r.Authorize(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("authorize %s: %w", ref, err)
		}
		creds = func(string) (string, string, error) { return user, pass, nil }
	}

	authorizer := docker.NewDockerAuthorizer(docker.WithAuthCreds(creds))
	return docker.NewResolver(docker.ResolverOptions{
		Hosts: docker.ConfigureDefaultRegistries(docker.WithAuthorizer(authorizer)),
	}), nil
}
