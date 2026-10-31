// Package gateway provides the AWS-compatible API gateway for the Spinifex platform.
package gateway

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	authlimit "github.com/mulgadc/spinifex/spinifex/ingress/aws/ratelimit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	bbotel "github.com/mulgadc/bluebottle/pkg/otelsetup"
	"github.com/mulgadc/bluebottle/pkg/ratelimit"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
	// The last discovered node count and when it was discovered, so the
	// discovery fan-out runs once per activeNodesTTL rather than once per
	// request that needs to know how many nodes to wait for.
	activeNodesMu    sync.RWMutex
	activeNodesCount int
	activeNodesAt    time.Time
	InternalSuffix   string // Internal DNS suffix for AWS-parity endpoints (e.g. spinifex.internal)
	IAMService       iamdomain.IAMService
	STSService       stsdomain.STSService
	// AnonymousSTS serves the unsigned STS actions that authenticate by a token
	// in the body. Nil answers them with InternalError.
	AnonymousSTS stsawsapi.AnonymousFunc

	// Credentials verifies SigV4 credentials. Nil fails every request closed.
	Credentials credential.Verifier
	RateLimiter *authlimit.AuthRateLimiter // Per-IP auth failure rate limiter
	Throttler   *ratelimit.Throttler       // Per-account+action API request throttler
	// Admission decides whether a request's account may proceed. Nil fails every
	// authenticated request closed with InternalError.
	Admission AccountAdmission
	// Policy decides authorization for a verified principal. Nil fails every
	// authorization closed with InternalError.
	Policy PolicyEvaluator
	// Readiness reports whether the message bus is reachable. Nil skips the
	// early refusal in the auth middleware and makes the service gate answer
	// cluster-unavailable.
	Readiness Readiness
	Version   string // Build-time version string (set from cmd.Version)
	Commit    string // Build-time commit hash (set from cmd.Commit)
	// OIDC serves the unauthenticated EKS OIDC discovery and JWKS endpoints. Nil
	// answers them with a 500 (e.g. in unit tests of unrelated routes).
	OIDC OIDCPublisher
	// RegistryHandler serves the registry host's /v2 subtree. It sits outside the
	// SigV4 group and the AWS seam, which ADR-0002:S5 keeps off the registry. Nil
	// answers 501.
	RegistryHandler http.Handler

	// Services holds the signed AWS services dispatched through the
	// registration seam. Nil means nothing is registered, so every signed
	// request is refused as an unserved service.
	Services *dispatch.Registry
}

// selector answers service selection over Services. It holds only the one
// pointer, so it is built per call and needs no cache.
func (gw *GatewayConfig) selector() *dispatch.Selector {
	return dispatch.NewSelector(gw.Services, nil)
}

// registered reports whether svc is served through the registration seam. A
// nil registry (no registrations) always reports false.
func (gw *GatewayConfig) registered(svc string) (*dispatch.Entry, bool) {
	return gw.selector().Lookup(svc)
}

// ValidateServices fails if the registry's service names are inconsistent.
func (gw *GatewayConfig) ValidateServices() error {
	return gw.selector().Validate()
}

// registeredActorFunc returns the lazy Actor resolver a registered
// dispatcher's Invocation carries. It reads the SigV4 identity context through
// the caller ARN the verifier built, so nothing is computed until called.
func (gw *GatewayConfig) registeredActorFunc(r *http.Request) dispatch.ActorFunc {
	return func() (dispatch.Actor, error) {
		return callerActor(r.Context())
	}
}

// callerActor is the verified caller as the seam hands it to a service. The
// actor is fully populated even when err is set: err reports that no caller ARN
// could be composed, so a reader that needs no ARN may ignore it.
func callerActor(ctx context.Context) (dispatch.Actor, error) {
	p, _ := reqctx.Principal(ctx)
	callerARN := p.CallerARN
	var err error
	if callerARN == "" {
		slog.Error("caller ARN could not be composed", "principalType", p.PrincipalType)
		err = errors.New(awserrors.ErrorInternalError)
	}
	actor := dispatch.Actor{
		AccountID:     p.AccountID,
		CallerARN:     callerARN,
		PrincipalType: p.PrincipalType,
		AccessKeyID:   reqctx.AccessKeyID(ctx),
		UserID:        p.UserID,
	}
	if p.PrincipalType == principalTypeAssumedRole {
		actor.PrincipalARN = p.UnderlyingRoleARN
		actor.SessionName = p.Identity
	} else {
		actor.PrincipalARN = callerARN
		actor.UserName = p.Identity
	}
	return actor, err
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
			acct := callerOf(r).AccountID
			if acct == "" {
				return "", fmt.Errorf("account-id missing from request context")
			}
			return acct, nil
		},
		func(r *http.Request) (string, error) {
			action := reqctx.Action(r.Context())
			if action != "" {
				return action, nil
			}
			return "unknown", nil
		},
	}
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
	svc := reqctx.Service(r.Context())

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
	action := reqctx.Action(r.Context())
	slog.Info("Request", "service", svc, "action", action, "method", r.Method, "path", r.URL.Path)

	if err != nil {
		slog.Error("GetService error", "error", err)
		gw.ErrorHandler(w, r, err)
		return
	}

	// Fail fast when NATS is down; every NATS-bound handler would otherwise hang
	// until per-call timeout.
	gw.fillDefaultCapabilities()
	if gw.Readiness == nil || !gw.Readiness.Available() {
		gw.writeClusterUnavailable(w, r, svc)
		return
	}

	entry, _ := gw.registered(svc)
	err = gw.dispatchRegistered(entry, w, r)

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
		AccountID: callerOf(r).AccountID,
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
	return reqctx.Service(r.Context())
}

func (gw *GatewayConfig) GetService(r *http.Request) (string, error) {
	svc := reqctx.Service(r.Context())
	if svc == "" {
		return "", errors.New(awserrors.ErrorAuthFailure)
	}
	if _, ok := gw.registered(svc); !ok {
		slog.Debug("Unsupported service", "service", svc)
		return "", errors.New(awserrors.ErrorUnsupportedOperation)
	}
	return svc, nil
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

// checkPolicyResourcesWithKeys is checkPolicyResources with action-scoped
// condition keys layered over the request's own.
func (gw *GatewayConfig) checkPolicyResourcesWithKeys(r *http.Request, service, action string, resources []string, actionKeys iampolicy.ConditionKeys) error {
	// Every dispatcher — query-protocol and REST-JSON alike — reaches this
	// point with its resolved action, so telemetry enrichment lives here.
	recordResolvedAction(r.Context(), service, action)

	gw.fillDefaultCapabilities()
	if gw.Policy == nil {
		slog.Error("checkPolicy: policy evaluator not available", "service", service, "action", action)
		return errors.New(awserrors.ErrorInternalError)
	}

	caller, authenticated := reqctx.Principal(r.Context())
	if !authenticated {
		slog.Warn("checkPolicy: request carries no auth context", "service", service, "action", action)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	if caller.Identity == "" {
		slog.Warn("checkPolicy: authenticated request carries no identity name",
			"service", service, "action", action)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	if caller.AccountID == "" {
		slog.Error("checkPolicy: no account ID in auth context", "user", caller.Identity)
		return errors.New(awserrors.ErrorInternalError)
	}

	keys := gw.Policy.RequestConditionKeys(r, caller)
	maps.Copy(keys, actionKeys)
	return gw.evaluatePrincipalPolicyResources(caller, policy.IAMAction(service, action), resources, keys)
}

// evaluatePrincipalPolicyResources resolves policies once and evaluates every
// resource in the request against that same snapshot.
func (gw *GatewayConfig) evaluatePrincipalPolicyResources(
	principal credential.Principal, iamAction string, resources []string, keys iampolicy.ConditionKeys,
) error {
	return gw.Policy.Evaluate(principal, iamAction, resources, keys)
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
		recordResolvedAction(ctx, reqctx.Service(ctx), reqctx.Action(ctx))

		span := trace.SpanFromContext(ctx)
		if acct := callerOf(r).AccountID; acct != "" {
			span.SetAttributes(attribute.String("aws.account_id", acct))
		}
		if region := reqctx.Region(ctx); region != "" {
			span.SetAttributes(attribute.String("aws.region", region))
		}
		next.ServeHTTP(w, r)
	})
}
