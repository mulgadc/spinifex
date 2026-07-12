// Package runtime is the ecs-agent's container-runtime seam. The agent pulls
// task images and (from Sprint 4e) runs containers through an ImagePuller; the
// real implementation drives containerd, but the interface lets tests and the
// scheduler-only build run against a fake.
package runtime

import "context"

// PullSpec describes one image to pull.
type PullSpec struct {
	// Ref is the full image reference, e.g.
	// "123456789012.dkr.ecr.us-east-1.spinifex.internal/app:latest".
	Ref string
}

// Image identifies a pulled image in the runtime's content store.
type Image struct {
	Ref    string
	Digest string
	Size   int64
}

// Resolver supplies registry credentials for a given image reference. The
// ecs-agent backs this with ECR GetAuthorizationToken (see ecrresolver.go).
type Resolver interface {
	// Authorize returns the username, password and registry endpoint to use for
	// ref. An empty user/pass means anonymous pull.
	Authorize(ctx context.Context, ref string) (user, pass, endpoint string, err error)
}

// ImagePuller pulls images into the local runtime. Close releases the runtime
// client connection.
type ImagePuller interface {
	Pull(ctx context.Context, spec PullSpec, r Resolver) (Image, error)
	Close() error
}

// RunSpec describes a single container the agent must create and start. v1 runs
// the container in the host network namespace (bridge/CNI task networking lands
// in a later sprint); Labels carry the mulga.ecs.* task identity for the reboot
// reconciler.
type RunSpec struct {
	Image   string
	Command []string
	Env     map[string]string
	Labels  map[string]string
	// NetnsPath, when set, joins the container to a pre-built network namespace
	// (awsvpc task ENI). Empty keeps the container in the host (VM) netns —
	// bridge/host mode behaviour.
	NetnsPath string
	// GPU is the whole-GPU count requested for this container. Not yet consumed
	// by the runtime — CDI device injection lands in a later Epic C task.
	GPU int
}

// RunStatus is a finished container's outcome.
type RunStatus struct {
	ExitCode int
}

// Container is a container the runtime already knows about, discovered by List.
// Labels carry the mulga.ecs.* task identity so the agent can re-adopt running
// containers after a restart; Running reports whether its task is live.
type Container struct {
	ID      string
	Labels  map[string]string
	Running bool
}

// Runner creates and starts containers from already-pulled images. id is a
// caller-unique container ID; Wait blocks until the container exits; Remove
// tears down the container + its task; List enumerates known containers so the
// reboot reconciler can re-adopt the ones still running.
type Runner interface {
	Run(ctx context.Context, id string, spec RunSpec) (containerID string, err error)
	Wait(ctx context.Context, containerID string) (RunStatus, error)
	Remove(ctx context.Context, containerID string) error
	List(ctx context.Context) ([]Container, error)
}

// Runtime is the full container runtime the ecs-agent drives: pull + run.
type Runtime interface {
	ImagePuller
	Runner
}
