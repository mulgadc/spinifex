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
//
// AccountID scopes the endpoint to a tenant; empty resolves to
// utils.GlobalAccountID, the shared platform endpoint every caller used
// before per-account endpoints existed. Pinned marks a created endpoint
// exempt from idle reaping and eviction.
type EnsureEndpointInput struct {
	ModelID   string `json:"model_id"`
	AccountID string `json:"account_id,omitempty"`
	Pinned    bool   `json:"pinned,omitempty"`
}

type EnsureEndpointOutput struct {
	Endpoint EndpointRecord `json:"endpoint"`
}

// DescribeEndpointInput looks up modelID's current endpoint record.
// AccountID scopes the lookup; empty resolves to utils.GlobalAccountID.
type DescribeEndpointInput struct {
	ModelID   string `json:"model_id"`
	AccountID string `json:"account_id,omitempty"`
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
// AccountID scopes the target; empty resolves to utils.GlobalAccountID.
type DeleteEndpointInput struct {
	ModelID   string `json:"model_id"`
	AccountID string `json:"account_id,omitempty"`
}

type DeleteEndpointOutput struct{}
