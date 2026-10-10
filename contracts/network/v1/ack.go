package networkv1

// AckEnvelope is the reply body on every request/reply route in this
// package: Success true with no error, or Success false with Error set.
// Fire-and-forget routes never populate it; there is no reply subject.
type AckEnvelope struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}
