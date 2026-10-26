package elbv2

import (
	"sync"
)

// mockSystemInstanceLauncher implements SystemInstanceLauncher for tests in
// service_impl_test.go and service_impl_vpc_test.go.
type mockSystemInstanceLauncher struct {
	mu             sync.Mutex
	launchCalls    []*SystemInstanceInput
	terminateCalls []string
	launchResult   *SystemInstanceOutput
	launchErr      error
	terminateErr   error
	terminateDone  chan struct{} // closed after TerminateSystemInstance completes
}

func (m *mockSystemInstanceLauncher) LaunchSystemInstance(input *SystemInstanceInput) (*SystemInstanceOutput, error) {
	m.launchCalls = append(m.launchCalls, input)
	return m.launchResult, m.launchErr
}

func (m *mockSystemInstanceLauncher) TerminateSystemInstance(instanceID string) error {
	m.mu.Lock()
	m.terminateCalls = append(m.terminateCalls, instanceID)
	m.mu.Unlock()
	if m.terminateDone != nil {
		close(m.terminateDone)
	}
	return m.terminateErr
}

// waitTerminate blocks until TerminateSystemInstance has been called.
func (m *mockSystemInstanceLauncher) waitTerminate() {
	if m.terminateDone != nil {
		<-m.terminateDone
	}
}

// Compile-time check that the mock satisfies the production interface.
var _ SystemInstanceLauncher = (*mockSystemInstanceLauncher)(nil)
