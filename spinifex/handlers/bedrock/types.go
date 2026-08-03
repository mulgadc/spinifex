package handlers_bedrock

// NATS subjects the daemon subscribes and NATSEndpointService forwards to,
// following the "eks.MethodName" naming EKS uses for its own internal
// (non-AWS-SDK-shaped) subjects.
const (
	SubjectEnsureEndpoint   = "bedrock.endpoint.ensure"
	SubjectDescribeEndpoint = "bedrock.endpoint.describe"
	SubjectListEndpoints    = "bedrock.endpoint.list"
	SubjectDeleteEndpoint   = "bedrock.endpoint.delete"
)

// EnsureEndpointInput requests that modelID have a running (or starting)
// serving endpoint. Idempotent: a model already STARTING or READY returns its
// current record without launching a second VM.
type EnsureEndpointInput struct {
	ModelID string `json:"model_id"`
}

type EnsureEndpointOutput struct {
	Endpoint EndpointRecord `json:"endpoint"`
}

// DescribeEndpointInput looks up modelID's current endpoint record.
type DescribeEndpointInput struct {
	ModelID string `json:"model_id"`
}

type DescribeEndpointOutput struct {
	Endpoint EndpointRecord `json:"endpoint"`
}

// ListEndpointsInput has no fields today; accountID comes from the NATS
// message header like every other handler, not the payload.
type ListEndpointsInput struct{}

type ListEndpointsOutput struct {
	Endpoints []EndpointRecord `json:"endpoints"`
}

// DeleteEndpointInput requests a READY endpoint move to DRAINING and its VM
// be torn down. Idempotent: an already-ABSENT endpoint returns success.
type DeleteEndpointInput struct {
	ModelID string `json:"model_id"`
}

type DeleteEndpointOutput struct{}
