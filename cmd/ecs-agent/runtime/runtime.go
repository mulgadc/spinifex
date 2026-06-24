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
