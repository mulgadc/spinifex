package dispatch

// XMLErrorShape selects the XML body an XML error renders in. The zero value
// is the generic REST-XML ErrorResponse.
type XMLErrorShape int

const (
	// XMLErrorShapeGeneric is the <ErrorResponse> envelope.
	XMLErrorShapeGeneric XMLErrorShape = iota
	// XMLErrorShapeEC2 is the EC2 <Response><Errors> envelope.
	XMLErrorShapeEC2
	// XMLErrorShapeS3 is the S3 <Error> envelope.
	XMLErrorShapeS3
)

// ServiceTraits are the behaviours the pipeline applies by credential scope
// without naming a service: the owning registration declares them, and the
// composition declares them for a scope it knows but does not serve.
type ServiceTraits struct {
	// XMLThrottleCode is the code of the XML throttle rejection. Unset, the
	// gateway default RequestLimitExceeded applies. XML envelopes only.
	XMLThrottleCode string
	// XMLErrorShape is the XML body the scope's errors render in.
	XMLErrorShape XMLErrorShape
}
