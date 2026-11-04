package instance

// OutstandingInstance is an instance of an account that is not yet fully torn
// down, with the state its record carries.
type OutstandingInstance struct {
	InstanceID string
	// State is the record's lifecycle state, such as running or terminated.
	State string
	// Teardown is the per-dependent progress (done, pending or failed) the
	// record carries. It is empty when no dependent has been recorded.
	Teardown map[string]string
}
