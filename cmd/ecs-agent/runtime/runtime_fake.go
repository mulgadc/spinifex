package runtime

import (
	"context"
	"fmt"
	"sync"
)

// FakePuller is an in-memory ImagePuller for tests and the scheduler-only build.
// It records pull requests and replays a programmed result or error.
type FakePuller struct {
	mu     sync.Mutex
	Pulls  []PullSpec
	Authzd []string
	Result Image
	Err    error
	Closed bool
	OnPull func(PullSpec) (Image, error)

	// Run bookkeeping.
	Runs     []RunSpec
	RunErr   error
	WaitCode int
	WaitErr  error
	Removed  []string
	OnRun    func(string, RunSpec) (string, error)
}

var (
	_ ImagePuller = (*FakePuller)(nil)
	_ Runner      = (*FakePuller)(nil)
)

// Pull records the spec, exercises the resolver, and returns the programmed
// result. If OnPull is set it takes precedence over Result/Err.
func (f *FakePuller) Pull(ctx context.Context, spec PullSpec, r Resolver) (Image, error) {
	f.mu.Lock()
	f.Pulls = append(f.Pulls, spec)
	f.mu.Unlock()

	if r != nil {
		user, _, _, err := r.Authorize(ctx, spec.Ref)
		if err != nil {
			return Image{}, fmt.Errorf("authorize %s: %w", spec.Ref, err)
		}
		f.mu.Lock()
		f.Authzd = append(f.Authzd, user)
		f.mu.Unlock()
	}

	if f.OnPull != nil {
		return f.OnPull(spec)
	}
	if f.Err != nil {
		return Image{}, f.Err
	}
	res := f.Result
	if res.Ref == "" {
		res.Ref = spec.Ref
	}
	return res, nil
}

// Close marks the puller closed.
func (f *FakePuller) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Closed = true
	return nil
}

// Run records the spec and returns id (or the programmed result/error).
func (f *FakePuller) Run(_ context.Context, id string, spec RunSpec) (string, error) {
	f.mu.Lock()
	f.Runs = append(f.Runs, spec)
	f.mu.Unlock()
	if f.OnRun != nil {
		return f.OnRun(id, spec)
	}
	if f.RunErr != nil {
		return "", f.RunErr
	}
	return id, nil
}

// Wait returns the programmed exit code/error.
func (f *FakePuller) Wait(_ context.Context, _ string) (RunStatus, error) {
	return RunStatus{ExitCode: f.WaitCode}, f.WaitErr
}

// Remove records the container ID.
func (f *FakePuller) Remove(_ context.Context, containerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Removed = append(f.Removed, containerID)
	return nil
}
