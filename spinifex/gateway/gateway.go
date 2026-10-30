package gateway

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	authlimit "github.com/mulgadc/spinifex/spinifex/ingress/aws/ratelimit"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	bbotel "github.com/mulgadc/bluebottle/pkg/otelsetup"
	"github.com/mulgadc/bluebottle/pkg/ratelimit"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	ochrevector "github.com/mulgadc/spinifex/spinifex/domains/ochre/vector"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/policy"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// contextKey is a typed key for request context values.
type contextKey string

const (
	ctxService   contextKey = "sigv4.service"
	ctxRegion    contextKey = "sigv4.region"
	ctxAction    contextKey = "sigv4.action"
	ctxQueryArgs contextKey = "sigv4.queryArgs"
)

// Values of callerPrincipal.principalType. Downstream handlers that interpret
// callerPrincipal.identity as an IAM user name MUST gate on principalTypeUser; otherwise a
// session whose SessionName collides with a user name inherits that user's policies.
const (
	principalTypeUser        = "user"
	principalTypeAssumedRole = "assumed-role"
	principalTypeRoot        = "root"
)

// GatewayConfig is the AWS API gateway's shared state: its NATS connection, region and cluster
// facts, auth and throttling collaborators, and lazily built caches. Its methods are the
// per-service request handlers.
type GatewayConfig struct {
	Debug          bool       `json:"debug"`
	DisableLogging bool       `json:"disable_logging"`
	NATSConn       *nats.Conn // Shared NATS connection for service communication
	Config         string     // Shared AWS Gateway config for S3 auth
	// RootCAs is the cluster CA pool, used to verify predastore meta nodes
	// dialed directly for GetStorageStatus. Nil disables that verification,
	// so admin storage status reports every meta node unreachable rather than
	// dialing with no trust root.
	RootCAs       *x509.CertPool
	ExpectedNodes int    // Number of expected spinifex nodes for multi-node operations
	Region        string // Region this gateway is running in
	// NodeIDs is the configured cluster node set — the keys of
	// config.ClusterConfig.Nodes — used to judge a fan-out's completeness by
	// responder identity rather than by count. Distinct from ExpectedNodes,
	// which stays a count for the callers that only want one.
	NodeIDs []string
	// The last discovered node count and when it was discovered, so the
	// discovery fan-out runs once per activeNodesTTL rather than once per
	// request that needs to know how many nodes to wait for.
	activeNodesMu    sync.RWMutex
	activeNodesCount int
	activeNodesAt    time.Time
	// The EC2 client-token KV binding, bound on first use and reused after, so
	// the creates that honour ClientToken do not rebind it per request.
	ec2TokenOnce   sync.Once
	ec2TokenKV     jetstream.KeyValue
	ec2TokenErr    error
	InternalSuffix string // Internal DNS suffix for AWS-parity endpoints (e.g. spinifex.internal)
	// RegistryPort is the gateway's advertised port, appended to the ECR
	// registry host so docker login/tag/push dial the right port. Empty or
	// "443" renders a port-less host (standard HTTPS parity).
	RegistryPort string
	// RegistryHost is the gateway's advertised registry host. When set, ECR URIs
	// use it (account comes from the auth token), so docker needs no DNS — it is
	// the same reachable, cert-covered host clients use for the AWS API. Empty
	// falls back to the per-account <acct>.dkr.ecr.<region>.<suffix> name.
	RegistryHost string
	AZ           string // Availability zone this gateway is running in
	// InstanceStatus backfills DescribeInstanceStatus for instances whose node
	// stopped answering, which would otherwise drop out of the answer entirely.
	// Zero value keeps the pre-existing fan-out-only behaviour.
	InstanceStatus ec2instanceapi.StatusSynthesis
	IAMService     iamdomain.IAMService
	STSService     stsdomain.STSService
	// AnonymousSTS serves the unsigned STS actions that authenticate by a token
	// in the body. Nil answers them with InternalError.
	AnonymousSTS stsawsapi.AnonymousFunc

	// Credentials verifies SigV4 credentials. Nil fails every request closed.
	Credentials credential.Verifier
	RateLimiter *authlimit.AuthRateLimiter // Per-IP auth failure rate limiter
	Throttler   *ratelimit.Throttler       // Per-account+action API request throttler
	// Admission decides whether a request's account may proceed. Nil builds one
	// over IAMService the first time the auth middleware is constructed.
	Admission *accountadmission.Admitter
	// Quota enforces per-account service quotas. Built unconditionally; a disabled
	// config yields a no-op Service whose Exempt always returns true. Nil only in
	// unit tests of unrelated routes, where no handler reaches the quota checks.
	Quota   *quota.Service
	Version string // Build-time version string (set from cmd.Version)
	Commit  string // Build-time commit hash (set from cmd.Commit)
	// OIDC serves the unauthenticated EKS OIDC discovery and JWKS endpoints. Nil
	// answers them with a 500 (e.g. in unit tests of unrelated routes).
	OIDC OIDCPublisher
	// RegistryHandler serves the registry host's /v2 subtree. It sits outside the
	// SigV4 group and the AWS seam, which ADR-0002:S5 keeps off the registry. Nil
	// answers 501.
	RegistryHandler http.Handler
	// BedrockCredentials resolves per-account provider API keys for bedrock
	// routes. Nil falls back to no external providers (self-host models only).
	BedrockCredentials *ochre.CredentialStore
	// BedrockEndpoints maps a self-hosted modelId to its OpenAI-compatible base
	// URL. These endpoints are pinned/always-resident, so this is static
	// config, and they resolve ahead of the dynamic registry so a pinned
	// endpoint bypasses the lifecycle entirely.
	BedrockEndpoints map[string]string
	// BedrockEndpointResolver resolves self-host endpoints through the daemon's
	// registry, requesting a launch for a model that has no endpoint yet. Nil
	// falls back to BedrockEndpoints alone, under which a model that was never
	// pinned is never reachable.
	BedrockEndpointResolver ochre.EndpointResolver
	// BedrockLoggingConfig persists per-account invocation-logging preferences
	// (PutModelInvocationLoggingConfiguration and friends). Nil falls back to
	// an unconfigured store, under which reads/writes error rather than panic.
	BedrockLoggingConfig *ochre.LoggingConfigStore
	// BedrockRecorder durably records every Bedrock invocation. Nil falls back
	// to a no-op recorder, so routes stay safe to exercise before the
	// invocation stream is wired in (e.g. unit tests of unrelated routes).
	BedrockRecorder ochre.Recorder
	// BedrockAccess resolves per-account model-access grants. Access is
	// deny-by-default, so nil means no account may list or invoke any model.
	// It is the AccessResolver interface rather than the concrete store so a
	// test can inject a fixed grant set without standing up JetStream.
	BedrockAccess ochre.AccessResolver
	// BedrockAccessAdmin is the writable side of the same grant store, used by
	// the spinifex admin actions. Nil disables grant administration, which is
	// how a gateway with an injected read-only resolver behaves.
	BedrockAccessAdmin *ochre.ModelAccessStore
	// BedrockProvisioned persists provisioned-throughput commitments and
	// drives the pinned endpoint underneath each one. Nil falls back to an
	// unconfigured store, under which reads/writes error rather than panic.
	BedrockProvisioned *ochre.ProvisionedStore
	// BedrockGuardrails persists guardrail control-plane records (CreateGuardrail
	// and friends). Nil falls back to an unconfigured store, under which
	// reads/writes error rather than panic.
	BedrockGuardrails *ochre.GuardrailStore
	// BedrockEmbedder drives guardrail topicPolicy's semantic match, reusing
	// the same embedding endpoint the Ochre KB path resolves models through.
	// Nil leaves topicPolicy on its literal (word-boundary) matcher alone.
	BedrockEmbedder ochre.Embedder

	// BedrockAgentKB and BedrockAgentDataSources persist bedrock-agent
	// knowledge-base and data-source resource metadata (D-arch: gateway-owned,
	// not the daemon-owned vector engine). Nil for either fails
	// BedrockAgent_Request with ServerInternal rather than panicking, the same
	// as an unconfigured gw.NATSConn does for every other service.
	BedrockAgentKB          *ochrevector.KBStore
	BedrockAgentDataSources *ochrevector.DataSourceStore
	// BedrockAgentVector forwards CreateIndex/DeleteIndex/Ingest/DescribeJob/
	// ListJobs calls to .9's daemon-side VectorService over NATS
	// (ochrevector.NewNATSVectorService). It is the interface, not
	// the concrete client, so a test can inject a fake without a live NATS
	// connection.
	BedrockAgentVector ochrevector.VectorService

	// Services holds the signed AWS services dispatched through the
	// registration seam. Nil means nothing is registered, so every request
	// falls through to the legacy switch in Request.
	Services *dispatch.Registry
}

// selector answers service selection over Services and the legacy tables. It
// holds only the two pointers, so it is built per call and needs no cache.
func (gw *GatewayConfig) selector() *dispatch.Selector {
	return dispatch.NewSelector(gw.Services, legacyServices{})
}

// legacyServices answers selection for services still on Request's switch.
type legacyServices struct{}

var _ dispatch.Legacy = legacyServices{}

func (legacyServices) Serves(svc string) bool { return supportedServices[svc] }

func (legacyServices) ResolveAction(r *http.Request, svc string) string {
	return resolveNonQueryAction(r, svc)
}

func (legacyServices) JSONErrors(svc string) bool { return jsonErrorService(svc) }

// registered reports whether svc is served through the registration seam. A
// nil registry (no registrations) always reports false.
func (gw *GatewayConfig) registered(svc string) (*dispatch.Entry, bool) {
	return gw.selector().Lookup(svc)
}

// ValidateServices fails if a name the registry serves is also served by the
// legacy supportedServices table, so no service is ever reachable down both
// dispatch paths at once.
func (gw *GatewayConfig) ValidateServices() error {
	return gw.selector().Validate()
}

// registeredActorFunc returns the lazy Actor resolver a registered
// dispatcher's Invocation carries. It reads the SigV4 identity context through
// the shared buildCallerARN helper, so nothing is computed until called.
func (gw *GatewayConfig) registeredActorFunc(r *http.Request) dispatch.ActorFunc {
	return func() (dispatch.Actor, error) {
		return callerActor(r.Context())
	}
}

// callerActor is the verified caller as the seam hands it to a service. The
// actor is fully populated even when err is set: err reports that no caller ARN
// could be composed, so a reader that needs no ARN may ignore it.
func callerActor(ctx context.Context) (dispatch.Actor, error) {
	p, _ := callerPrincipalFrom(ctx)
	callerARN, err := buildCallerARN(p.accountID, p.identity, p.principalType, p.assumedRoleARN, p.userARN)
	actor := dispatch.Actor{
		AccountID:     p.accountID,
		CallerARN:     callerARN,
		PrincipalType: p.principalType,
		AccessKeyID:   p.accessKeyID,
		UserID:        p.userID,
	}
	if p.principalType == principalTypeAssumedRole {
		actor.PrincipalARN = p.underlyingRoleARN
		actor.SessionName = p.identity
	} else {
		actor.PrincipalARN = callerARN
		actor.UserName = p.identity
	}
	return actor, err
}

var supportedServices = map[string]bool{
	"ec2":                   true,
	"bedrock":               true,
	"bedrock-runtime":       true,
	"bedrock-agent":         true,
	"bedrock-agent-runtime": true,
}

func (gw *GatewayConfig) SetupRoutes() http.Handler {
	var logLevel slog.Level

	if gw.Debug {
		logLevel = slog.LevelDebug
	} else if gw.DisableLogging {
		logLevel = slog.LevelError
	} else {
		logLevel = slog.LevelInfo
	}

	// Adjust the level only. Reinstalling the default logger here would drop the OTLP
	// bridge Init fanned on at startup, blinding the sink to every line after this.
	bbotel.SetLevel(logLevel)

	if gw.RateLimiter == nil {
		gw.RateLimiter = authlimit.NewAuthRateLimiter()
	}

	r := chi.NewRouter()

	r.Use(otelsetup.HTTPMiddleware("awsgw"))
	r.Use(audit.Middleware)

	if !gw.DisableLogging {
		r.Use(audit.RequestLogger)
	}

	// Anonymous STS (AssumeRoleWithWebIdentity) is dispatched ahead of SigV4 —
	// these calls carry a web-identity JWT, not AWS credentials.
	r.Use(gw.anonymousSTSInterceptor)

	// Unauthenticated OIDC discovery endpoints (IRSA) bypass auth and throttle.
	r.Group(func(pub chi.Router) {
		pub.Get("/oidc/eks/{region}/{accountID}/{clusterName}/.well-known/openid-configuration", gw.OIDCDiscoveryDocument)
		pub.Get("/oidc/eks/{region}/{accountID}/{clusterName}/keys", gw.OIDCJWKS)
	})

	// OCI Distribution registry (/v2/*). Token/host-authenticated rather than
	// SigV4-credential-scoped, so it mounts outside the SigV4 group.
	gw.mountRegistry(r)

	// Authenticated AWS API surface.
	r.Group(func(auth chi.Router) {
		auth.Use(gw.SigV4AuthMiddleware())
		auth.Use(traceActionEnricher)

		// Post-auth, per-account+action token bucket throttle.
		if gw.Throttler != nil {
			auth.Use(gw.Throttler.Middleware(
				gw.throttleKeyFuncs(),
				gw.writeThrottleError,
			))
		}

		// Private super-admin surface. A distinct path prefix rather than an
		// Action on the spinifex namespace, so the edge proxy can restrict it
		// by location without parsing a signed request body.
		auth.HandleFunc("/admin/{method}", gw.Admin_Request)

		auth.HandleFunc("/*", gw.Request)
	})

	return r
}

// throttleKeyFuncs returns the KeyFunc slice for the API throttle middleware,
// keyed by account-id and action from the SigV4 auth context.
func (gw *GatewayConfig) throttleKeyFuncs() []ratelimit.KeyFunc {
	return []ratelimit.KeyFunc{
		func(r *http.Request) (string, error) {
			acct := callerPrincipalOf(r).accountID
			if acct == "" {
				return "", fmt.Errorf("account-id missing from request context")
			}
			return acct, nil
		},
		func(r *http.Request) (string, error) {
			action, _ := r.Context().Value(ctxAction).(string)
			if action != "" {
				return action, nil
			}
			return "unknown", nil
		},
	}
}

// jsonErrorService reports whether svc returns AWS JSON 1.1 errors rather than
// XML. One source of truth so every error emitter agrees with ErrorHandler; an
// XML body to these clients is an unparseable "<?xml…" deserialization error.
func jsonErrorService(svc string) bool {
	switch svc {
	case "bedrock", "bedrock-runtime", "bedrock-agent", "bedrock-agent-runtime":
		return true
	}
	return false
}

// errorProtocol reports the JSON wire format svc's errors use; ok is false when
// they render as XML. A registered service's declared envelope wins.
func (gw *GatewayConfig) errorProtocol(svc string) (p envelope.Protocol, ok bool) {
	return gw.selector().ErrorProtocol(svc)
}

// writeJSONError renders a gateway-raised error in svc's JSON protocol. The
// code gets the common-error "Exception" suffix unless verbatim.
func (gw *GatewayConfig) writeJSONError(w http.ResponseWriter, p envelope.Protocol, svc string, e envelope.JSONError) {
	if entry, ok := gw.registered(svc); ok {
		if internal, own := entry.InternalErrorCode(); own && e.Code == awserrors.ErrorInternalError {
			e.Code, e.Verbatim = internal, true
		}
		e.QueryCode = entry.QueryErrorCode(e.Code)
		e.Namespace = entry.ErrorNamespace(e.Code)
	}
	envelope.WriteJSON(w, p, e)
}

// clusterUnavailableMsg is the 503 body when NATS is disconnected. Points
// operators at /local/status rather than leaving the AWS CLI hanging on timeouts.
const clusterUnavailableMsg = "cluster unavailable: NATS disconnected — check daemon /local/status"

// writeClusterUnavailable writes a 503 ServiceUnavailable in the service-appropriate
// format, carrying the /local/status hint in <Message>.
func (gw *GatewayConfig) writeClusterUnavailable(w http.ResponseWriter, r *http.Request, svc string) {
	requestID := uuid.NewV4().String()

	// JSON services get a body in their own protocol.
	if p, ok := gw.errorProtocol(svc); ok {
		gw.writeJSONError(w, p, svc, envelope.JSONError{
			Code:    awserrors.ErrorServiceUnavailable,
			Message: clusterUnavailableMsg,
			Status:  http.StatusServiceUnavailable,
		})
		return
	}

	xmlBody := xmlErrorBody(svc, awserrors.ErrorServiceUnavailable, clusterUnavailableMsg, requestID, r.URL.Path)

	w.Header().Set("Content-Type", envelope.XMLContentType)
	w.WriteHeader(http.StatusServiceUnavailable)
	if _, err := w.Write(xmlBody); err != nil {
		slog.Error("Failed to write cluster-unavailable response", "err", err)
	}
}

// writeThrottleError writes the service-appropriate throttle rejection response.
func (gw *GatewayConfig) writeThrottleError(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewV4().String()
	svc, _ := r.Context().Value(ctxService).(string)

	errorCode := awserrors.ErrorRequestLimitExceeded
	if svc == "iam" || svc == "sts" || svc == "eks" || svc == "route53" {
		errorCode = awserrors.ErrorThrottling
	}
	errorMsg := awserrors.LookupErrorMessage(svc, errorCode)

	// JSON services get a body in their own protocol.
	if p, ok := gw.errorProtocol(svc); ok {
		je := envelope.JSONError{Code: errorCode, Message: errorMsg.Message, Status: errorMsg.HTTPCode}
		if entry, reg := gw.registered(svc); reg {
			if spec, own := entry.ThrottleError(); own {
				je = envelope.JSONError{
					Code: spec.Code, Verbatim: true, Message: spec.Message,
					Status: spec.Status, Fields: spec.Fields,
				}
			}
		}
		gw.writeJSONError(w, p, svc, je)
		return
	}

	xmlErr := xmlErrorBody(svc, errorCode, errorMsg.Message, requestID, r.URL.Path)

	w.Header().Set("Content-Type", envelope.XMLContentType)
	w.WriteHeader(errorMsg.HTTPCode)
	if _, err := w.Write(xmlErr); err != nil {
		slog.Error("Failed to write throttle error response", "err", err)
	}
}

func (gw *GatewayConfig) Request(w http.ResponseWriter, r *http.Request) {
	svc, err := gw.GetService(r)
	action, _ := r.Context().Value(ctxAction).(string)
	slog.Info("Request", "service", svc, "action", action, "method", r.Method, "path", r.URL.Path)

	if err != nil {
		slog.Error("GetService error", "error", err)
		gw.ErrorHandler(w, r, err)
		return
	}

	// Fail fast when NATS is down; every NATS-bound handler would otherwise hang
	// until per-call timeout. Applies ahead of either dispatch path below.
	if gw.NATSConn == nil || !gw.NATSConn.IsConnected() {
		gw.writeClusterUnavailable(w, r, svc)
		return
	}

	if entry, ok := gw.registered(svc); ok {
		err = gw.dispatchRegistered(entry, w, r)
	} else {
		// Legacy dispatch path for every service not yet registered through
		// the seam. This switch must only shrink as services register.
		switch svc {
		case "ec2":
			err = gw.EC2_Request(w, r)
		case "bedrock":
			err = gw.Bedrock_Request(w, r)
		case "bedrock-runtime":
			err = gw.BedrockRuntime_Request(w, r)
		case "bedrock-agent":
			err = gw.BedrockAgent_Request(w, r)
		case "bedrock-agent-runtime":
			err = gw.BedrockAgentRuntime_Request(w, r)
		default:
			err = errors.New(awserrors.ErrorUnsupportedOperation)
		}
	}

	if err != nil {
		slog.Log(r.Context(), serviceErrorLevel(svc, err), "Service request error",
			"service", svc, "action", action, "error", err)
		gw.ErrorHandler(w, r, err)
	} else {
		slog.Info("Service request completed", "service", svc, "action", action)
	}
}

// serviceErrorLevel reports the level a dispatch failure belongs at, taken from
// the status the caller is about to be given. A 4xx is the client's own
// request, and DryRunOperation is a 412 that AWS documents as the success
// answer to a dry run, so only a 5xx is a fault of ours.
func serviceErrorLevel(svc string, err error) slog.Level {
	code, _, ok := awserrors.ResolveErrorDetail(err)
	if !ok {
		return slog.LevelError
	}
	status := awserrors.LookupErrorMessage(svc, code).HTTPCode
	if status >= 400 && status < 500 {
		return slog.LevelInfo
	}
	return slog.LevelError
}

// dispatchRegistered hands the authenticated request to a registered service
// with the caller's account, the gateway Region and ingress-owned
// authorization and actor capabilities.
func (gw *GatewayConfig) dispatchRegistered(entry *dispatch.Entry, w http.ResponseWriter, r *http.Request) error {
	return entry.Dispatch(w, dispatch.Invocation{
		Request:   r,
		AccountID: callerPrincipalOf(r).accountID,
		Service:   requestService(r),
		Region:    gw.Region,
		Authorize: func(service, action string, resources []string, keys iampolicy.ConditionKeys) error {
			return gw.checkPolicyResourcesWithKeys(r, service, action, resources, keys)
		},
		Actor: gw.registeredActorFunc(r),
	})
}

// requestService is the service the request's credential scope names.
func requestService(r *http.Request) string {
	svc, _ := r.Context().Value(ctxService).(string)
	return svc
}

func (gw *GatewayConfig) GetService(r *http.Request) (string, error) {
	svc, ok := r.Context().Value(ctxService).(string)
	if !ok {
		return "", errors.New(awserrors.ErrorAuthFailure)
	}
	// A registered service needs no bedrock sub-service rewriting: that
	// rewrite exists only for the legacy "bedrock" signing-name split below.
	if _, ok := gw.registered(svc); ok {
		return svc, nil
	}
	// The whole Bedrock family shares the SigV4 signing name "bedrock"; the
	// request path is the only discriminator, since the gateway serves one endpoint.
	if svc == "bedrock" {
		svc = resolveBedrockSubservice(r.URL.Path)
	}
	if !supportedServices[svc] {
		slog.Debug("Unsupported service", "service", svc)
		return "", errors.New(awserrors.ErrorUnsupportedOperation)
	}
	return svc, nil
}

// resolveBedrockSubservice maps a bedrock-family request path to its real
// sub-service, the only discriminator since all four share one signing name.
// Shared by GetService (post-auth dispatch) and SigV4 auth (action resolution).
func resolveBedrockSubservice(path string) string {
	switch {
	case strings.HasPrefix(path, "/model/") || strings.HasPrefix(path, "/guardrail/"):
		return "bedrock-runtime"
	case path == "/retrieveAndGenerate" || (strings.HasPrefix(path, "/knowledgebases/") && strings.HasSuffix(path, "/retrieve")):
		return "bedrock-agent-runtime"
	case strings.HasPrefix(path, "/knowledgebases/"):
		return "bedrock-agent"
	default:
		return "bedrock"
	}
}

// readBoundedBody reads a request body under the same cap the signed path
// applies, so a body read ahead of the policy gate can neither be used to
// bypass the gate nor to exhaust memory. On the signed path sigv4.Parse has
// already buffered and rewound these bytes, so this re-reads a buffer.
func readBoundedBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return body, nil
}

// checkPolicy evaluates IAM policies against the literal resource "*", so it
// grants account-wide: a caller's resource-scoped statements do not participate
// at all, neither a Deny that fences a resource nor an Allow that names one.
// Prefer checkPolicyResources with the request's real ARNs: where the
// identifier arrives in a JSON body, read the body ahead of the gate and
// resolve it there rather than settling for "*".
func (gw *GatewayConfig) checkPolicy(r *http.Request, service, action string) error {
	return gw.checkPolicyResources(r, service, action, []string{"*"})
}

// checkPolicyResources evaluates every resource against one resolved policy
// snapshot. Used when one API request authorizes multiple resource ARNs.
func (gw *GatewayConfig) checkPolicyResources(r *http.Request, service, action string, resources []string) error {
	return gw.checkPolicyResourcesWithKeys(r, service, action, resources, nil)
}

// Service principals an iam:PassRole check reports as iam:PassedToService.
const (
	ec2ServicePrincipal      = "ec2.amazonaws.com"
	ecsTasksServicePrincipal = "ecs-tasks.amazonaws.com"
	eksServicePrincipal      = "eks.amazonaws.com"
)

// checkPassRole enforces iam:PassRole on roleARN. iam:PassedToService is fixed
// by the call site handing the role over, never read from the request.
func (gw *GatewayConfig) checkPassRole(r *http.Request, roleARN, passedTo string) error {
	return gw.checkPolicyResourcesWithKeys(r, "iam", "PassRole", []string{roleARN},
		iampolicy.ConditionKeys{iampolicy.KeyPassedToService: passedTo})
}

// checkPolicyResourcesWithKeys is checkPolicyResources with action-scoped
// condition keys layered over the request's own.
func (gw *GatewayConfig) checkPolicyResourcesWithKeys(r *http.Request, service, action string, resources []string, actionKeys iampolicy.ConditionKeys) error {
	// Every dispatcher — query-protocol and REST-JSON alike — reaches this
	// point with its resolved action, so telemetry enrichment lives here.
	recordResolvedAction(r.Context(), service, action)

	if gw.IAMService == nil {
		slog.Error("checkPolicy: IAM service not available", "service", service, "action", action)
		return errors.New(awserrors.ErrorInternalError)
	}

	caller, authenticated := callerPrincipalFrom(r.Context())
	if !authenticated {
		slog.Warn("checkPolicy: request carries no auth context", "service", service, "action", action)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	if caller.identity == "" {
		slog.Warn("checkPolicy: authenticated request carries no identity name",
			"service", service, "action", action)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	if caller.accountID == "" {
		slog.Error("checkPolicy: no account ID in auth context", "user", caller.identity)
		return errors.New(awserrors.ErrorInternalError)
	}

	principal := caller.policyPrincipal()
	keys := requestConditionKeys(r, principal)
	maps.Copy(keys, actionKeys)
	return gw.evaluatePrincipalPolicyResources(principal, policy.IAMAction(service, action), resources, keys)
}

// credential projects the gateway's principal onto the credential vocabulary the
// shared policy evaluator takes.
func (p principalContext) credential() credential.Principal {
	return credential.Principal{
		Identity:          p.identity,
		AccountID:         p.accountID,
		PrincipalType:     p.principalType,
		AssumedRoleARN:    p.assumedRoleARN,
		AssumedRoleID:     p.assumedRoleID,
		UnderlyingRoleARN: p.underlyingRoleARN,
		UserID:            p.userID,
		UserARN:           p.userARN,
	}
}

// policyResolveRetryBase scales the backoff between policy-resolve attempts
// after a transient NATS error. A var only so tests can shorten it.
var policyResolveRetryBase = 200 * time.Millisecond

// requestConditionKeys resolves the request's IAM condition keys through the shared
// policy evaluator package.
func requestConditionKeys(r *http.Request, principal principalContext) iampolicy.ConditionKeys {
	return policyauth.RequestConditionKeys(r, principal.credential())
}

// evaluatePrincipalPolicyResources resolves policies once and evaluates every
// resource in the request against that same snapshot.
func (gw *GatewayConfig) evaluatePrincipalPolicyResources(
	principal principalContext, iamAction string, resources []string, keys iampolicy.ConditionKeys,
) error {
	return policyauth.New(gw.IAMService, policyauth.WithRetryBase(policyResolveRetryBase)).Evaluate(principal.credential(), iamAction, resources, keys)
}

func (gw *GatewayConfig) ErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	svc, _ := gw.GetService(r)
	slog.Debug("ErrorHandler", "service", svc, "error", err.Error())

	var requestId = uuid.NewV4().String()
	code, message, exists := awserrors.ResolveErrorDetail(err)
	if !exists {
		slog.Warn("Unknown error code", "error", err.Error())
		code = awserrors.ErrorInternalError
		message = ""
	}

	// LookupErrorMessage resolves per-service wording first (e.g. ACM vs EKS
	// both using "ResourceInUseException"); a message the producing call site
	// supplied via awserrors.Errorf then takes priority over either default.
	errorMsg := awserrors.LookupErrorMessage(svc, code)
	if message != "" {
		errorMsg.Message = message
	}
	if errorMsg.HTTPCode == 0 {
		errorMsg.HTTPCode = 500
	}

	// A 503 the producing call site knows is a bounded, self-clearing
	// condition (e.g. the embedder warm-up window) carries a suggested
	// Retry-After so SDK clients back off on their own cadence instead of
	// hammering the endpoint every request.
	if d, ok := awserrors.ResolveRetryAfter(err); ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())))
	}

	// JSON services answer in their own protocol; query/XML services fall
	// through.
	if p, ok := gw.errorProtocol(svc); ok {
		slog.Debug("Generated JSON error response", "service", svc, "error", err, "code", code, "requestId", requestId)
		gw.writeJSONError(w, p, svc, envelope.JSONError{
			Code: code, Verbatim: p != envelope.ProtocolJSON11,
			Message: errorMsg.Message, Status: errorMsg.HTTPCode,
			Fields: awserrors.ResolveFields(err),
		})
		return
	}

	xmlError := xmlErrorBody(svc, code, errorMsg.Message, requestId, r.URL.Path)

	slog.Debug("Generated error response", "error", err, "code", code, "xml", string(xmlError), "requestId", requestId)

	w.Header().Set("Content-Type", envelope.XMLContentType)
	w.WriteHeader(errorMsg.HTTPCode)
	if _, err := w.Write(xmlError); err != nil {
		slog.Error("Failed to write error response", "err", err)
	}
}

// readQueryArgs returns parsed query args from context (set by SigV4) or falls
// back to parsing the body (unauthenticated/test paths only).
func readQueryArgs(r *http.Request) (map[string]string, error) {
	if args, ok := r.Context().Value(ctxQueryArgs).(map[string]string); ok {
		return args, nil
	}
	// Same cap as the signed path: this fallback only runs when the context
	// carries no pre-parsed args, so the body has not been bounded upstream.
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return ParseAWSQueryArgs(string(body))
}

// ParseAWSQueryArgs parses an AWS query-protocol body. Returns an error on
// invalid percent-encoding so callers can surface MalformedQueryString.
func ParseAWSQueryArgs(body string) (map[string]string, error) {
	return query.ParseArgs(body)
}

// xmlErrorBody renders an error in the XML envelope svc's clients expect. The
// one place the service-to-envelope mapping lives, so a service cannot be
// added to some emitters and missed by others. JSON services never reach
// here — every caller checks jsonErrorService first.
func xmlErrorBody(svc, code, message, requestID, resource string) []byte {
	switch svc {
	case "s3":
		return envelope.S3Body(code, message, requestID, resource)
	case "ec2", "spinifex", "":
		// Both speak the Action-parameter query protocol, and their clients
		// parse the EC2 <Response><Errors> shape, not the generic ErrorResponse.
		// Empty means the signature never parsed, so no service is known yet;
		// keep EC2's shape there rather than changing a path this is not about.
		return envelope.EC2Body(code, message, requestID)
	default:
		// iam, sts, elasticloadbalancing, rds, and any unenumerated or
		// unserved scope: the generic REST-XML ErrorResponse envelope, never
		// EC2's shape, which a REST-XML client cannot deserialize.
		return envelope.IAMBody(code, message, requestID)
	}
}

// How long a discovered node count is reused before it is gathered again.
// Membership does not change per request, and re-deriving it on every call put
// the discovery fan-out's whole timeout in front of every API call it fronts.
const activeNodesTTL = 5 * time.Second

// discoverActiveNodesTimeout is how long the discovery fan-out collects replies.
// A var only so tests can shorten it.
var discoverActiveNodesTimeout = 500 * time.Millisecond

// DiscoverActiveNodes discovers the number of active spinifex daemon nodes in the
// cluster by publishing a discovery request and counting unique responses. It
// carries the request context so the discovery fan-out joins the caller's trace.
// Returns the number of active nodes (minimum 1 if fallback is needed).
//
// The result is cached for activeNodesTTL. The gather itself stays unbounded:
// it counts whoever answers, which may legitimately exceed the configured node
// count during a grow, and bounding it by that count would stop early and drop
// a live node's resources from every fan-out that follows.
func (gw *GatewayConfig) DiscoverActiveNodes(ctx context.Context) int {
	if gw.NATSConn == nil {
		slog.WarnContext(ctx, "DiscoverActiveNodes: NATS connection not available, using ExpectedNodes fallback", "fallback", gw.ExpectedNodes)
		return gw.ExpectedNodes
	}

	if count, ok := gw.cachedActiveNodes(); ok {
		return count
	}

	frames, _, err := natsmsg.Gather(ctx, gw.NATSConn, clusterv1.NodesDiscoverSubject, []byte("{}"),
		natsmsg.GatherOpts{Timeout: discoverActiveNodesTimeout})
	if err != nil {
		slog.ErrorContext(ctx, "DiscoverActiveNodes: fan-out failed, using ExpectedNodes fallback", "err", err, "fallback", gw.ExpectedNodes)
		return gw.ExpectedNodes
	}

	nodesSeen := make(map[string]bool)
	for _, frame := range frames {
		var response clusterv1.NodeDiscoverResponse
		if err := json.Unmarshal(frame.Data, &response); err != nil {
			slog.DebugContext(ctx, "DiscoverActiveNodes: Failed to unmarshal response", "err", err)
			continue
		}
		nodesSeen[response.Node] = true
	}

	activeNodes := len(nodesSeen)
	if activeNodes == 0 {
		slog.WarnContext(ctx, "DiscoverActiveNodes: No nodes responded, using ExpectedNodes fallback", "fallback", gw.ExpectedNodes)
		return gw.ExpectedNodes
	}

	gw.rememberActiveNodes(activeNodes)
	slog.DebugContext(ctx, "DiscoverActiveNodes: Discovered active nodes", "count", activeNodes)
	return activeNodes
}

// cachedActiveNodes returns a count discovered within the TTL. Only a real
// discovery is cached: a fallback is not evidence of anything and caching one
// would make a momentary NATS problem outlive itself.
func (gw *GatewayConfig) cachedActiveNodes() (int, bool) {
	gw.activeNodesMu.RLock()
	defer gw.activeNodesMu.RUnlock()
	if gw.activeNodesCount == 0 || time.Since(gw.activeNodesAt) > activeNodesTTL {
		return 0, false
	}
	return gw.activeNodesCount, true
}

func (gw *GatewayConfig) rememberActiveNodes(count int) {
	gw.activeNodesMu.Lock()
	defer gw.activeNodesMu.Unlock()
	gw.activeNodesCount = count
	gw.activeNodesAt = time.Now()
}

// recordResolvedAction renames the current span to service.action and tags
// aws.service/aws.action. Called from both SigV4 auth and checkPolicyResource,
// so it must be idempotent — the last call before the response wins.
func recordResolvedAction(ctx context.Context, service, action string) {
	if action == "" {
		return
	}
	span := trace.SpanFromContext(ctx)
	name := action
	if service != "" {
		name = service + "." + action
		span.SetAttributes(attribute.String("aws.service", service))
	}
	span.SetName(name)
	span.SetAttributes(attribute.String("aws.action", action))
	otelsetup.SetRequestAction(ctx, name)
	audit.From(ctx).SetAction(service, action)
}

// traceActionEnricher renames the server span to the resolved SigV4
// service.Action and tags account/region once auth populated the context.
// A dispatcher lacking its own action source (none currently do) would get
// the same treatment later, from checkPolicyResource.
func traceActionEnricher(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		action, _ := ctx.Value(ctxAction).(string)
		svc, _ := ctx.Value(ctxService).(string)
		recordResolvedAction(ctx, svc, action)

		span := trace.SpanFromContext(ctx)
		if acct := callerPrincipalOf(r).accountID; acct != "" {
			span.SetAttributes(attribute.String("aws.account_id", acct))
		}
		if region, _ := ctx.Value(ctxRegion).(string); region != "" {
			span.SetAttributes(attribute.String("aws.region", region))
		}
		next.ServeHTTP(w, r)
	})
}
