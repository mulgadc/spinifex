package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"
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
	Waited   []string
	Removed  []string
	OnRun    func(string, RunSpec) (string, error)

	// Stop bookkeeping: StopTimeouts pairs the container ID with the timeout it
	// was stopped with, in call order, so a test can assert both the target and
	// the enforced stopTimeout without a fake containerd.
	Stopped      []string
	StopTimeouts []time.Duration
	StopErr      error

	// List bookkeeping: Containers is replayed by List; ListErr forces a failure,
	// limited to the first ListErrFor calls when that is non-zero, so a test can
	// model a runtime that recovers. Listed and ListCalls record the calls.
	Containers []Container
	ListErr    error
	ListErrFor int
	Listed     bool
	ListCalls  int
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

// Wait records the container ID and returns the programmed exit code/error.
func (f *FakePuller) Wait(_ context.Context, containerID string) (RunStatus, error) {
	f.mu.Lock()
	f.Waited = append(f.Waited, containerID)
	f.mu.Unlock()
	return RunStatus{ExitCode: f.WaitCode}, f.WaitErr
}

// Stop records the container ID and the timeout it was stopped with.
func (f *FakePuller) Stop(_ context.Context, containerID string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Stopped = append(f.Stopped, containerID)
	f.StopTimeouts = append(f.StopTimeouts, timeout)
	return f.StopErr
}

// Remove records the container ID.
func (f *FakePuller) Remove(_ context.Context, containerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Removed = append(f.Removed, containerID)
	return nil
}

// Waits returns a copy of the container IDs passed to Wait, for assertions.
func (f *FakePuller) Waits() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Waited...)
}

// RunCalls returns a copy of the specs passed to Run, for assertions made while
// the agent's goroutines may still be live.
func (f *FakePuller) RunCalls() []RunSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RunSpec(nil), f.Runs...)
}

// Reaped returns a copy of the container IDs passed to Stop.
func (f *FakePuller) Reaped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Stopped...)
}

// List replays the programmed container set (or error).
func (f *FakePuller) List(_ context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Listed = true
	f.ListCalls++
	if f.ListErr != nil && (f.ListErrFor == 0 || f.ListCalls <= f.ListErrFor) {
		return nil, f.ListErr
	}
	return f.Containers, nil
}

// Lists reports how many times List was called.
func (f *FakePuller) Lists() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ListCalls
}
