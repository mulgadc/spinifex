// Package codexgatetest is a throwaway for the Codex review live test; never merged.
package codexgatetest

import (
	"context"
	"errors"
	"time"
)

// Volumes is the subset of the volume service the reaper uses.
type Volumes interface {
	Detach(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
}

// ReapStuckDetach detaches a volume, and deletes it when the detach does not finish in time.
func ReapStuckDetach(ctx context.Context, v Volumes, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := v.Detach(ctx, id)
	if errors.Is(err, context.DeadlineExceeded) {
		return v.Delete(context.Background(), id)
	}
	return err
}
