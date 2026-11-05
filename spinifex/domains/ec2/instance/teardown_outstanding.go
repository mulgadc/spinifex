package instance

import "context"

// OutstandingInstance is an instance of an account that is not yet fully torn
// down, with the state its record carries.
type OutstandingInstance struct {
	InstanceID string `json:"instance_id"`
	// State is the record's lifecycle state, such as running or terminated.
	State string `json:"state"`
	// Teardown is the per-dependent progress (done, pending or failed) the
	// record carries. It is empty when no dependent has been recorded.
	Teardown map[string]string `json:"teardown,omitempty"`
}

// TeardownOutstandingInput is the empty request of the outstanding-teardown
// read; the account comes from the message header.
type TeardownOutstandingInput struct{}

// TeardownOutstandingOutput carries every instance not yet fully torn down.
type TeardownOutstandingOutput struct {
	Instances []OutstandingInstance `json:"instances"`
}

// AccountTeardownOutstandingRPC serves AccountTeardownOutstanding over the
// message bus, so a process without the instance record store can read it.
func (s *InstanceServiceImpl) AccountTeardownOutstandingRPC(_ context.Context, _ *TeardownOutstandingInput, accountID string) (*TeardownOutstandingOutput, error) {
	outstanding, err := s.AccountTeardownOutstanding(accountID)
	if err != nil {
		return nil, err
	}
	return &TeardownOutstandingOutput{Instances: outstanding}, nil
}
