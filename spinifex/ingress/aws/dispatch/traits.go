package dispatch

// ServiceTraits are the behaviours the pipeline applies by credential scope
// without naming a service: the owning registration declares them.
type ServiceTraits struct {
	// XMLThrottleCode is the code of the XML throttle rejection. Unset, the
	// gateway default RequestLimitExceeded applies. XML envelopes only.
	XMLThrottleCode string
}
