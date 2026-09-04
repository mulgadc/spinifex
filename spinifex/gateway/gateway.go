package gateway

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	bbotel "github.com/mulgadc/bluebottle/pkg/otelsetup"
	"github.com/mulgadc/bluebottle/pkg/ratelimit"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/accountteardown"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
	"github.com/mulgadc/spinifex/spinifex/gateway/policy"
	gateway_sts "github.com/mulgadc/spinifex/spinifex/gateway/sts"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// contextKey is a typed key for request context values.
type contextKey string

const (
	ctxIdentity       contextKey = "sigv4.identity"
	ctxAccountID      contextKey = "sigv4.accountId"
	ctxService        contextKey = "sigv4.service"
	ctxRegion         contextKey = "sigv4.region"
	ctxAccessKey      contextKey = "sigv4.accessKey"
	ctxAction         contextKey = "sigv4.action"
	ctxQueryArgs      contextKey = "sigv4.queryArgs"
	ctxPrincipalType  contextKey = "sigv4.principalType"
	ctxAssumedRoleARN contextKey = "sigv4.assumedRoleARN"
	ctxAssumedRoleID  contextKey = "sigv4.assumedRoleID"
	// ctxUserID carries aws:userid, resolved once by the SigV4 middleware so
	// every policy check in a request evaluates the same value.
	ctxUserID contextKey = "sigv4.userID"
	// ctxUnderlyingRoleARN carries the IAM role ARN backing an assumed-role session.
	// Policy enforcement resolves the role name from this, never from ctxIdentity
	// (attacker-influenced RoleSessionName).
	ctxUnderlyingRoleARN contextKey = "sigv4.underlyingRoleARN"

	// ctxTargetAccount carries the accountID parsed from a registry host
	// ({accountID}.dkr.ecr.{region}.{suffix}) by the host-routing middleware.
	ctxTargetAccount contextKey = "host.targetAccount"
	// ctxTargetRegion carries the region parsed from the same registry host.
	ctxTargetRegion contextKey = "host.targetRegion"

	// ctxAuthPrincipal carries the verified ECR token subject (principal ARN).
	// The resolved account is stashed via gateway_ecr.WithAuthAccount so the
	// registry package can read it without sharing this package's key type.
	ctxAuthPrincipal contextKey = "ecr.authPrincipal"
	// ctxECRPrincipal carries the principalContext resolveECRPrincipal rebuilt
	// from current IAM/STS state for the request's ECR token. The operation-
	// authorization middleware reads this to run the same policy evaluator
	// SigV4 requests use; it is never populated for non-ECR routes.
	ctxECRPrincipal contextKey = "ecr.principal"
)

// Values stored under ctxPrincipalType. Downstream handlers that interpret
// ctxIdentity as an IAM user name MUST gate on principalTypeUser; otherwise a
// session whose SessionName collides with a user name inherits that user's policies.
const (
	principalTypeUser        = "user"
	principalTypeAssumedRole = "assumed-role"
	principalTypeRoot        = "root"
)

// Values GatewayConfig.DescribeSource resolves to. DescribeSourceFanout is
// today's behaviour and the default; an absent or unrecognised configured
// value falls back to it rather than failing the gateway to start.
const (
	DescribeSourceFanout = "fanout"
	DescribeSourceShadow = "shadow"
	DescribeSourceCache  = "cache"
)

// describeShadowTimeout bounds one shadow comparison. It runs detached from
// the request it shadows (which may already have finished), so it needs its
// own deadline rather than inheriting one that could already be cancelled.
const describeShadowTimeout = 5 * time.Second

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
	InstanceStatus gateway_ec2_instance.StatusSynthesis
	// DescribeSource selects what DescribeInstances answers from. Resolved
	// once at startup (see config.AWSGWConfig.DescribeSource) to one of the
	// DescribeSource* constants below; never re-read after that, matching
	// every other gateway setting and the fact that Reload is a no-op.
	DescribeSource string
	// DescribeCache is the cache-served describe path DescribeSource=cache
	// and DescribeSource=shadow read from. Nil means no cache is wired, so a
	// gateway built without one behaves exactly as it did before this
	// existed.
	DescribeCache gateway_ec2_instance.CacheReader
	// DescribeShadow runs the cache path alongside an already-served
	// fan-out answer when DescribeSource is "shadow", comparing and logging
	// divergences without ever altering what was served. Nil disables
	// shadow mode regardless of DescribeSource, matching a gateway built
	// without DescribeCache.
	DescribeShadow *gateway_ec2_instance.ShadowComparator
	IAMService     handlers_iam.IAMService
	// BucketStore reaps a tenant's S3 buckets during account teardown. It
	// signs with the config service credential, which predastore already
	// trusts to reach any bucket, so this grants enumeration, not access.
	// Nil makes DeleteAccount refuse rather than tear down around the data.
	BucketStore accountteardown.BucketStore
	STSService  handlers_sts.STSService
	RateLimiter *AuthRateLimiter     // Per-IP auth failure rate limiter
	Throttler   *ratelimit.Throttler // Per-account+action API request throttler
	// accountStatus caches which accounts are ACTIVE, so enforcing account
	// status does not add a KV read to every authenticated request.
	accountStatus *accountStatusCache
	// Quota enforces per-account service quotas. Built unconditionally; a disabled
	// config yields a no-op Service whose Exempt always returns true. Nil only in
	// unit tests of unrelated routes, where no handler reaches the quota checks.
	Quota   *handlers_quota.Service
	Version string // Build-time version string (set from cmd.Version)
	Commit  string // Build-time commit hash (set from cmd.Commit)
	// ECRRegistry serves the OCI Distribution v2 (/v2/*) surface. Nil falls back
	// to the 501 stub (e.g. in unit tests of unrelated routes).
	ECRRegistry *gateway_ecr.Registry
	// ECRTokenIssuer mints GetAuthorizationToken JWTs; ECRTokenVerifier validates
	// them on /v2/*. Both nil disables the auth bridge (registry mounts open, as
	// in unit tests of unrelated routes).
	ECRTokenIssuer   *gateway_ecrauth.Issuer
	ECRTokenVerifier *gateway_ecrauth.Verifier
	// BedrockCredentials resolves per-account provider API keys for bedrock
	// routes. Nil falls back to no external providers (self-host models only).
	BedrockCredentials *gateway_bedrock.CredentialStore
	// BedrockEndpoints maps a self-hosted modelId to its OpenAI-compatible base
	// URL. These endpoints are pinned/always-resident, so this is static
	// config, and they resolve ahead of the dynamic registry so a pinned
	// endpoint bypasses the lifecycle entirely.
	BedrockEndpoints map[string]string
	// BedrockEndpointResolver resolves self-host endpoints through the daemon's
	// registry, requesting a launch for a model that has no endpoint yet. Nil
	// falls back to BedrockEndpoints alone, under which a model that was never
	// pinned is never reachable.
	BedrockEndpointResolver gateway_bedrock.EndpointResolver
	// BedrockLoggingConfig persists per-account invocation-logging preferences
	// (PutModelInvocationLoggingConfiguration and friends). Nil falls back to
	// an unconfigured store, under which reads/writes error rather than panic.
	BedrockLoggingConfig *gateway_bedrock.LoggingConfigStore
	// BedrockRecorder durably records every Bedrock invocation. Nil falls back
	// to a no-op recorder, so routes stay safe to exercise before the
	// invocation stream is wired in (e.g. unit tests of unrelated routes).
	BedrockRecorder gateway_bedrock.Recorder
	// BedrockAccess resolves per-account model-access grants. Access is
	// deny-by-default, so nil means no account may list or invoke any model.
	// It is the AccessResolver interface rather than the concrete store so a
	// test can inject a fixed grant set without standing up JetStream.
	BedrockAccess gateway_bedrock.AccessResolver
	// BedrockAccessAdmin is the writable side of the same grant store, used by
	// the spinifex admin actions. Nil disables grant administration, which is
	// how a gateway with an injected read-only resolver behaves.
	BedrockAccessAdmin *gateway_bedrock.ModelAccessStore
	// BedrockProvisioned persists provisioned-throughput commitments and
	// drives the pinned endpoint underneath each one. Nil falls back to an
	// unconfigured store, under which reads/writes error rather than panic.
	BedrockProvisioned *gateway_bedrock.ProvisionedStore
	// BedrockGuardrails persists guardrail control-plane records (CreateGuardrail
	// and friends). Nil falls back to an unconfigured store, under which
	// reads/writes error rather than panic.
	BedrockGuardrails *gateway_bedrock.GuardrailStore
	// BedrockEmbedder drives guardrail topicPolicy's semantic match, reusing
	// the same embedding endpoint the Ochre KB path resolves models through.
	// Nil leaves topicPolicy on its literal (word-boundary) matcher alone.
	BedrockEmbedder gateway_bedrock.Embedder

	// SignupMaxAccounts caps how many accounts /admin/CreateAccount will allow
	// to exist. Zero means uncapped, which is the behaviour of every cluster
	// that has not opted into self-service signup.
	SignupMaxAccounts int

	// BedrockAgentKB and BedrockAgentDataSources persist bedrock-agent
	// knowledge-base and data-source resource metadata (D-arch: gateway-owned,
	// not the daemon-owned vector engine). Nil for either fails
	// BedrockAgent_Request with ServerInternal rather than panicking, the same
	// as an unconfigured gw.NATSConn does for every other service.
	BedrockAgentKB          *handlers_ochrevector.KBStore
	BedrockAgentDataSources *handlers_ochrevector.DataSourceStore
	// BedrockAgentVector forwards CreateIndex/DeleteIndex/Ingest/DescribeJob/
	// ListJobs calls to .9's daemon-side VectorService over NATS
	// (handlers_ochrevector.NewNATSVectorService). It is the interface, not
	// the concrete client, so a test can inject a fake without a live NATS
	// connection.
	BedrockAgentVector handlers_ochrevector.VectorService
}

var supportedServices = map[string]bool{
	"ec2":                   true,
	"iam":                   true,
	"sts":                   true,
	"elasticloadbalancing":  true,
	"eks":                   true,
	"ecs":                   true,
	"ecr":                   true,
	"acm":                   true,
	"rds":                   true,
	"tagging":               true,
	"spinifex":              true,
	"bedrock":               true,
	"bedrock-runtime":       true,
	"bedrock-agent":         true,
	"bedrock-agent-runtime": true,
}

// EC2ErrorResponse is the EC2 query-API error envelope.
// aws-sdk-go v1's ec2query handler rejects the IAM-style <ErrorResponse> envelope
// with SerializationError, so EC2 errors must use <Response><Errors>...</Errors></Response>.
type EC2ErrorResponse struct {
	XMLName   xml.Name  `xml:"Response"`
	Errors    EC2Errors `xml:"Errors"`
	RequestID string    `xml:"RequestID"`
}

type EC2Errors struct {
	Error ErrorDetail `xml:"Error"`
}

type ErrorDetail struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// S3ErrorResponse is the S3 REST error envelope: a flat <Error> document rather
// than the query-API wrapper. SDKs look for a top-level <Error><Code> on an S3
// response and report an empty code for anything else.
type S3ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
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
		gw.RateLimiter = NewAuthRateLimiter()
	}

	r := chi.NewRouter()

	r.Use(otelsetup.HTTPMiddleware("awsgw"))
	r.Use(requestAuditMiddleware)

	if !gw.DisableLogging {
		r.Use(slogRequestLogger)
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
	gw.mountOCIRegistry(r)

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
			acct, ok := r.Context().Value(ctxAccountID).(string)
			if !ok || acct == "" {
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

// eksJSONContentType is the AWS REST-JSON 1.1 content type EKS clients expect.
const eksJSONContentType = "application/x-amz-json-1.1"

// jsonErrorService reports whether svc returns AWS JSON 1.1 errors rather than
// XML. One source of truth so every error emitter agrees with ErrorHandler; an
// XML body to these clients is an unparseable "<?xml…" deserialization error.
func jsonErrorService(svc string) bool {
	switch svc {
	case "eks", "ecr", "acm", "ecs", "tagging",
		"bedrock", "bedrock-runtime", "bedrock-agent", "bedrock-agent-runtime":
		return true
	}
	return false
}

// clusterUnavailableMsg is the 503 body when NATS is disconnected. Points
// operators at /local/status rather than leaving the AWS CLI hanging on timeouts.
const clusterUnavailableMsg = "cluster unavailable: NATS disconnected — check daemon /local/status"

// writeClusterUnavailable writes a 503 ServiceUnavailable in the service-appropriate
// format, carrying the /local/status hint in <Message>.
func (gw *GatewayConfig) writeClusterUnavailable(w http.ResponseWriter, r *http.Request, svc string) {
	requestID := uuid.NewV4().String()

	// AWS JSON 1.1 services (EKS/ECS/bedrock family, …) get a JSON body.
	if jsonErrorService(svc) {
		body := GenerateEKSErrorResponse(awserrors.ErrorServiceUnavailable, clusterUnavailableMsg, requestID)
		w.Header().Set("Content-Type", eksJSONContentType)
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := w.Write(body); err != nil {
			slog.Error("Failed to write JSON cluster-unavailable response", "err", err)
		}
		return
	}

	xmlBody := xmlErrorBody(svc, awserrors.ErrorServiceUnavailable, clusterUnavailableMsg, requestID, r.URL.Path)

	w.Header().Set("Content-Type", "application/xml")
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
	if svc == "iam" || svc == "sts" || svc == "eks" {
		errorCode = awserrors.ErrorThrottling
	}
	errorMsg := awserrors.ErrorLookup[errorCode]

	// AWS JSON 1.1 services (EKS/ECS/bedrock family, …) get a JSON body.
	if jsonErrorService(svc) {
		body := GenerateEKSErrorResponse(errorCode, errorMsg.Message, requestID)
		w.Header().Set("Content-Type", eksJSONContentType)
		w.WriteHeader(errorMsg.HTTPCode)
		if _, err := w.Write(body); err != nil {
			slog.Error("Failed to write JSON throttle error response", "err", err)
		}
		return
	}

	xmlErr := xmlErrorBody(svc, errorCode, errorMsg.Message, requestID, r.URL.Path)

	w.Header().Set("Content-Type", "application/xml")
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
	// until per-call timeout.
	if gw.NATSConn == nil || !gw.NATSConn.IsConnected() {
		gw.writeClusterUnavailable(w, r, svc)
		return
	}

	switch svc {
	case "ec2":
		err = gw.EC2_Request(w, r)
	case "iam":
		err = gw.IAM_Request(w, r)
	case "sts":
		err = gw.STS_Request(w, r)
	case "elasticloadbalancing":
		err = gw.ELBv2_Request(w, r)
	case "eks":
		err = gw.EKS_Request(w, r)
	case "bedrock":
		err = gw.Bedrock_Request(w, r)
	case "bedrock-runtime":
		err = gw.BedrockRuntime_Request(w, r)
	case "bedrock-agent":
		err = gw.BedrockAgent_Request(w, r)
	case "bedrock-agent-runtime":
		err = gw.BedrockAgentRuntime_Request(w, r)
	case "ecs":
		err = gw.ECS_Request(w, r)
	case "ecr":
		err = gw.ECR_Request(w, r)
	case "acm":
		err = gw.ACM_Request(w, r)
	case "rds":
		err = gw.RDS_Request(w, r)
	case "tagging":
		err = gw.Tagging_Request(w, r)
	case "spinifex":
		err = gw.Spinifex_Request(w, r)
	default:
		err = errors.New(awserrors.ErrorUnsupportedOperation)
	}

	if err != nil {
		slog.Error("Service request error", "service", svc, "action", action, "error", err)
		gw.ErrorHandler(w, r, err)
	} else {
		slog.Info("Service request completed", "service", svc, "action", action)
	}
}

func (gw *GatewayConfig) GetService(r *http.Request) (string, error) {
	svc, ok := r.Context().Value(ctxService).(string)
	if !ok {
		return "", errors.New(awserrors.ErrorAuthFailure)
	}
	// The whole Bedrock family (bedrock, bedrock-runtime, bedrock-agent,
	// bedrock-agent-runtime) shares the SigV4 signing name "bedrock" -- real
	// AWS separates them by endpoint hostname, but the gateway serves one
	// endpoint, so the request path is the only discriminator available here.
	// /model/... and singular /guardrail/... are exclusive to bedrock-runtime;
	// control-plane guardrail CRUD uses the plural /guardrails, so the
	// prefixes never collide. Retrieve's /knowledgebases/{id}/retrieve and
	// RetrieveAndGenerate's /retrieveAndGenerate are checked ahead of the
	// bedrock-agent /knowledgebases/... prefix, since Retrieve's own path is
	// itself a /knowledgebases/... path.
	if svc == "bedrock" {
		switch {
		case strings.HasPrefix(r.URL.Path, "/model/") || strings.HasPrefix(r.URL.Path, "/guardrail/"):
			svc = "bedrock-runtime"
		case r.URL.Path == "/retrieveAndGenerate" || (strings.HasPrefix(r.URL.Path, "/knowledgebases/") && strings.HasSuffix(r.URL.Path, "/retrieve")):
			svc = "bedrock-agent-runtime"
		case strings.HasPrefix(r.URL.Path, "/knowledgebases/"):
			svc = "bedrock-agent"
		}
	}
	if !supportedServices[svc] {
		slog.Debug("Unsupported service", "service", svc)
		return "", errors.New(awserrors.ErrorUnsupportedOperation)
	}
	return svc, nil
}

// isNATSTransient reports whether err represents a transient NATS/JetStream
// failure that may resolve after cluster leader election completes.
func isNATSTransient(err error) bool {
	return err != nil && (errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoStreamResponse))
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
	// Every dispatcher — query-protocol and REST-JSON alike — reaches this
	// point with its resolved action, so telemetry enrichment lives here.
	recordResolvedAction(r.Context(), service, action)

	if gw.IAMService == nil {
		slog.Error("checkPolicy: IAM service not available", "service", service, "action", action)
		return errors.New(awserrors.ErrorInternalError)
	}

	identityVal := r.Context().Value(ctxIdentity)
	if identityVal == nil {
		slog.Warn("checkPolicy: request carries no auth context", "service", service, "action", action)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	identity, ok := identityVal.(string)
	if !ok {
		slog.Error("checkPolicy: identity has unexpected type", "type", fmt.Sprintf("%T", identityVal))
		return errors.New(awserrors.ErrorInternalError)
	}
	if identity == "" {
		slog.Warn("checkPolicy: authenticated request carries no identity name",
			"service", service, "action", action)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("checkPolicy: no account ID in auth context", "user", identity)
		return errors.New(awserrors.ErrorInternalError)
	}

	principal := principalContext{
		identity:          identity,
		accountID:         accountID,
		principalType:     mustCtxString(r, ctxPrincipalType),
		assumedRoleARN:    mustCtxString(r, ctxAssumedRoleARN),
		assumedRoleID:     mustCtxString(r, ctxAssumedRoleID),
		underlyingRoleARN: mustCtxString(r, ctxUnderlyingRoleARN),
		userID:            mustCtxString(r, ctxUserID),
	}
	return gw.evaluatePrincipalPolicyResources(principal, policy.IAMAction(service, action), resources,
		requestConditionKeys(r, principal))
}

// requestConditionKeys resolves the IAM condition context keys available on the
// AWS API path. s3:prefix has no meaning here and is deliberately absent, so a
// policy conditioned on it does not fire.
//
// Every key is omitted rather than set empty when unknown: an empty value reads
// as a real value that matches nothing, and on a Deny that silently widens
// access instead of narrowing it.
func requestConditionKeys(r *http.Request, principal principalContext) iampolicy.ConditionKeys {
	keys := iampolicy.ConditionKeys{
		iampolicy.KeySecureTransport: strconv.FormatBool(r.TLS != nil),
	}
	// aws:username is user-only in AWS. A role session's identity is the
	// caller-chosen RoleSessionName, so gating authorization on it here would
	// let any principal that may assume the role satisfy the condition at will.
	if principal.principalType == principalTypeUser && principal.identity != "" {
		keys[iampolicy.KeyUsername] = principal.identity
	}
	if principal.accountID != "" {
		keys[iampolicy.KeyPrincipalAccount] = principal.accountID
	}
	// Resolved once where the principal was, so a per-check lookup cannot leave
	// one check in a request evaluating against a different context than the next.
	if principal.userID != "" {
		keys[iampolicy.KeyUserID] = principal.userID
	}
	// Derived from the request rather than read from the context: the OCI
	// registry chain never runs SigV4AuthMiddleware, so a context-carried
	// address would be absent there and every aws:SourceIp condition inert.
	if ip := utils.ClientIP(r.RemoteAddr); ip != "" {
		keys[iampolicy.KeySourceIP] = ip
	}
	return keys
}

// principalUserID resolves aws:userid: an IAM user's unique ID, the role ID and
// session name STS minted for a role session, or the account ID for root. Both
// halves of a session's ID come from the resolved role, so unlike aws:username
// it is not caller-chosen.
//
// A principal with no ID on record returns empty and the door omits the key. A
// dependency fault returns InternalError instead: authorizing against a context
// missing the key silently narrows an Allow and widens a Deny.
func (gw *GatewayConfig) principalUserID(principal principalContext) (string, error) {
	if principal.identity == "" || principal.accountID == "" {
		return "", nil
	}
	userID, err := gateway_sts.ResolveCallerUserID(principal.accountID, principal.principalType,
		principal.identity, principal.assumedRoleID, gw.IAMService)
	switch {
	case err == nil:
		if userID == "" {
			slog.Warn("aws:userid unavailable: principal record carries no user ID",
				"accountID", principal.accountID, "identity", principal.identity,
				"principalType", principal.principalType)
		}
		return userID, nil
	case strings.Contains(err.Error(), awserrors.ErrorIAMNoSuchEntity):
		slog.Warn("aws:userid unavailable: no such IAM user",
			"accountID", principal.accountID, "user", principal.identity)
		return "", nil
	default:
		slog.Error("aws:userid: IAM dependency fault, refusing to authorize on a degraded context",
			"accountID", principal.accountID, "identity", principal.identity, "err", err)
		return "", errors.New(awserrors.ErrorInternalError)
	}
}

// mustCtxString reads a string context value, defaulting to "" for an absent
// or wrong-typed key rather than panicking.
func mustCtxString(r *http.Request, key contextKey) string {
	v, _ := r.Context().Value(key).(string)
	return v
}

// evaluatePrincipalPolicyResources resolves policies once and evaluates every
// resource in the request against that same snapshot.
func (gw *GatewayConfig) evaluatePrincipalPolicyResources(
	principal principalContext, iamAction string, resources []string, keys iampolicy.ConditionKeys,
) error {
	if gw.IAMService == nil {
		slog.Error("evaluatePrincipalPolicy: IAM service not available", "action", iamAction)
		return errors.New(awserrors.ErrorInternalError)
	}

	// Each branch resolves the policy resolver and log identity for its principal
	// type. Identity-sensitive decisions (root bypass, resolver selection) are
	// fully inside their branch so an assumed-role SessionName of "root" cannot
	// reach the user-branch root short-circuit.
	var resolve func() ([]handlers_iam.PolicyDocument, error)
	var logIdentity string

	switch principal.principalType {
	case principalTypeUser:
		if principal.identity == "root" && principal.accountID == utils.GlobalAccountID {
			// Global root bypass — user branch only.
			return nil
		}
		resolve = func() ([]handlers_iam.PolicyDocument, error) {
			return gw.IAMService.GetUserPolicies(principal.accountID, principal.identity)
		}
		logIdentity = principal.identity
	case principalTypeAssumedRole:
		// Resolve by the session's underlying role, never by SessionName (attacker-influenced).
		// A missing/legacy, cross-account, or malformed ARN fails closed with AccessDenied.
		roleAcct, roleName, perr := auth.ParseRoleARN(principal.underlyingRoleARN)
		if perr != nil || roleAcct != principal.accountID {
			slog.Warn("evaluatePrincipalPolicy: unresolvable or cross-account assumed-role principal denied",
				"underlyingRoleARN", principal.underlyingRoleARN,
				"accountID", principal.accountID,
				"action", iamAction,
				"err", perr)
			return errors.New(awserrors.ErrorAccessDenied)
		}
		resolve = func() ([]handlers_iam.PolicyDocument, error) {
			return gw.IAMService.GetRolePolicies(principal.accountID, roleName)
		}
		logIdentity = principal.assumedRoleARN
	default:
		slog.Error("evaluatePrincipalPolicy: unknown principal type", "principalType", principal.principalType)
		return errors.New(awserrors.ErrorInternalError)
	}

	// Resolve policies, retrying transient NATS errors. Fail-closed on non-transient errors.
	var policies []handlers_iam.PolicyDocument
	var err error
	for attempt := range 3 {
		policies, err = resolve()
		if err == nil || !isNATSTransient(err) {
			break
		}
		if attempt < 2 {
			slog.Debug("evaluatePrincipalPolicy: transient NATS error, retrying",
				"identity", logIdentity, "attempt", attempt+1, "err", err)
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
		}
	}
	if err != nil {
		slog.Error("evaluatePrincipalPolicy: failed to resolve policies", "identity", logIdentity, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}

	for _, resource := range resources {
		if iampolicy.EvaluateWithKeys(iamAction, resource, policies, keys) == iampolicy.Deny {
			slog.Info("evaluatePrincipalPolicy: access denied",
				"identity", logIdentity, "action", iamAction, "resource", resource)
			return &identityPolicyDenialError{
				principal: principal, logIdentity: logIdentity,
				action: iamAction, resource: resource,
			}
		}
	}
	return nil
}

// identityPolicyDenialError keeps shared authorization failures opaque. A
// caller that proves its resource is non-sensitive may opt into details.
type identityPolicyDenialError struct {
	principal   principalContext
	logIdentity string
	action      string
	resource    string
}

func (d *identityPolicyDenialError) Error() string {
	return awserrors.ErrorAccessDenied
}

func (d *identityPolicyDenialError) detailedError() error {
	callerARN, err := buildCallerARN(d.principal.accountID, d.principal.identity,
		d.principal.principalType, d.principal.assumedRoleARN)
	if err != nil {
		callerARN = d.logIdentity
	}
	return awserrors.Errorf(awserrors.ErrorAccessDenied,
		"User: %s is not authorized to perform: %s on resource: %s",
		callerARN, d.action, d.resource)
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

	// EKS, ECR, ACM, ECS, tagging, and the bedrock family use AWS JSON 1.1;
	// query/XML services fall through.
	if jsonErrorService(svc) {
		body := GenerateEKSErrorResponse(code, errorMsg.Message, requestId)
		slog.Debug("Generated JSON error response", "service", svc, "error", err, "code", code, "json", string(body), "requestId", requestId)
		w.Header().Set("Content-Type", eksJSONContentType)
		w.WriteHeader(errorMsg.HTTPCode)
		if _, err := w.Write(body); err != nil {
			slog.Error("Failed to write EKS error response", "err", err)
		}
		return
	}

	xmlError := xmlErrorBody(svc, code, errorMsg.Message, requestId, r.URL.Path)

	slog.Debug("Generated error response", "error", err, "code", code, "xml", string(xmlError), "requestId", requestId)

	w.Header().Set("Content-Type", "application/xml")
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
func ParseAWSQueryArgs(query string) (map[string]string, error) {
	values, err := url.ParseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("invalid AWS query string: %w", err)
	}

	params := make(map[string]string, len(values))
	for key, vs := range values {
		// The query protocol indexes repeated parameters (Filter.1.Value.1), so a
		// bare duplicate key only arrives from a non-conforming client. Take the
		// last occurrence.
		params[key] = vs[len(vs)-1]
	}
	return params, nil
}

func GenerateEC2ErrorResponse(code, message, requestID string) (output []byte) {
	errorXml := EC2ErrorResponse{
		Errors: EC2Errors{
			Error: ErrorDetail{
				Code:    code,
				Message: message,
			},
		},
		RequestID: requestID,
	}

	output, err := xml.MarshalIndent(errorXml, "", "  ")

	if err != nil {
		slog.Error("Failed to build XML", "error", err)
		return []byte(xml.Header + `<Response><Errors><Error><Code>InternalError</Code><Message>Internal error</Message></Error></Errors><RequestID>` + requestID + `</RequestID></Response>`)
	}

	// Add XML header
	output = append([]byte(xml.Header), output...)

	return output
}

// IAMErrorResponse is the IAM/STS error XML envelope.
type IAMErrorResponse struct {
	XMLName   xml.Name       `xml:"ErrorResponse"`
	Error     IAMErrorDetail `xml:"Error"`
	RequestID string         `xml:"RequestId"`
}

type IAMErrorDetail struct {
	Type    string `xml:"Type"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func GenerateIAMErrorResponse(code, message, requestID string) (output []byte) {
	errorXml := IAMErrorResponse{
		Error: IAMErrorDetail{
			Type:    "Sender",
			Code:    code,
			Message: message,
		},
		RequestID: requestID,
	}

	output, err := xml.MarshalIndent(errorXml, "", "  ")
	if err != nil {
		slog.Error("Failed to build IAM error XML", "error", err)
		return []byte(xml.Header + "<ErrorResponse><Error><Type>Sender</Type><Code>InternalError</Code><Message>Internal error</Message></Error><RequestId>" + requestID + "</RequestId></ErrorResponse>")
	}

	output = append([]byte(xml.Header), output...)
	return output
}

// GenerateS3ErrorResponse builds the flat S3 REST error document. resource is
// the request path and is omitted when empty.
func GenerateS3ErrorResponse(code, message, requestID, resource string) (output []byte) {
	errorXml := S3ErrorResponse{
		Code:      code,
		Message:   message,
		Resource:  resource,
		RequestID: requestID,
	}

	output, err := xml.MarshalIndent(errorXml, "", "  ")
	if err != nil {
		slog.Error("Failed to build S3 error XML", "error", err)
		return []byte(xml.Header + "<Error><Code>InternalError</Code><Message>Internal error</Message><RequestId>" + requestID + "</RequestId></Error>")
	}

	output = append([]byte(xml.Header), output...)
	return output
}

// xmlErrorBody renders an error in the XML envelope svc's clients expect. The
// one place the service-to-envelope mapping lives, so a service cannot be added
// to some emitters and missed by others. JSON services never reach here — every
// caller checks jsonErrorService first.
func xmlErrorBody(svc, code, message, requestID, resource string) []byte {
	switch svc {
	case "s3":
		return GenerateS3ErrorResponse(code, message, requestID, resource)
	case "iam", "sts", "elasticloadbalancing", "rds":
		return GenerateIAMErrorResponse(code, message, requestID)
	default:
		// ec2, account, spinifex, and any scope the gateway does not serve.
		return GenerateEC2ErrorResponse(code, message, requestID)
	}
}

// How long a discovered node count is reused before it is gathered again.
// Membership does not change per request, and re-deriving it on every call put
// the discovery fan-out's whole timeout in front of every API call it fronts.
const activeNodesTTL = 5 * time.Second

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

	frames, _, err := utils.Gather(ctx, gw.NATSConn, "spinifex.nodes.discover", []byte("{}"),
		utils.GatherOpts{Timeout: 500 * time.Millisecond})
	if err != nil {
		slog.ErrorContext(ctx, "DiscoverActiveNodes: fan-out failed, using ExpectedNodes fallback", "err", err, "fallback", gw.ExpectedNodes)
		return gw.ExpectedNodes
	}

	nodesSeen := make(map[string]bool)
	for _, frame := range frames {
		var response types.NodeDiscoverResponse
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

// runDescribeShadow starts a detached shadow comparison for a just-served
// DescribeInstances answer when DescribeSource is "shadow". It is a no-op
// under every other source, and under shadow itself when no cache or
// comparator is wired, so the cache path never runs unless shadow mode is
// explicitly configured with both.
//
// out is snapshotted synchronously, before this returns: it keeps being
// mutated in place after the handler returns (marshalEC2Response normalizes
// nil slices on it), so nothing crossing into the goroutine may hold a
// reference into it. The goroutine itself runs on a context detached from
// the request — which may finish, cancelling its own context, before the
// comparison does — with its own bounded deadline.
func (gw *GatewayConfig) runDescribeShadow(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string, out *ec2.DescribeInstancesOutput) {
	if gw.DescribeSource != DescribeSourceShadow || gw.DescribeShadow == nil || gw.DescribeCache == nil {
		return
	}

	snap := gateway_ec2_instance.SnapshotDescribeOutput(out)
	shadowCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), describeShadowTimeout)
	go func() {
		defer cancel()
		gw.DescribeShadow.Run(shadowCtx, gw.DescribeCache, gw.InstanceStatus.Liveness, input, accountID, gw.AZ, snap)
	}()
}

// recordResolvedAction renames the current span to service.action, tags it
// with aws.service/aws.action, and updates the request's metric action name.
// Query-protocol services resolve their action during SigV4 auth, before
// dispatch; REST-JSON services (path-routed or X-Amz-Target-routed) only know
// it once checkPolicyResource runs. Called from both paths, so it must be
// idempotent — the last call before the response is written wins.
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
	auditFrom(ctx).setAction(service, action)
}

// traceActionEnricher renames the server span to the resolved SigV4
// service.Action and tags account/region once auth populated the context.
// Only fires here for query-protocol services, whose action is known before
// dispatch; REST-JSON services get the same treatment later, from
// checkPolicyResource, once their dispatcher resolves the action.
func traceActionEnricher(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		action, _ := ctx.Value(ctxAction).(string)
		svc, _ := ctx.Value(ctxService).(string)
		recordResolvedAction(ctx, svc, action)

		span := trace.SpanFromContext(ctx)
		if acct, _ := ctx.Value(ctxAccountID).(string); acct != "" {
			span.SetAttributes(attribute.String("aws.account_id", acct))
		}
		if region, _ := ctx.Value(ctxRegion).(string); region != "" {
			span.SetAttributes(attribute.String("aws.region", region))
		}
		next.ServeHTTP(w, r)
	})
}

// slogRequestLogger is a middleware that logs each request via slog. The audit
// record carries the caller and the auth verdict onto the same line, so a
// failing request is answerable without joining it to a separate auth log.
func slogRequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		logRequest(r, ww.Status(), time.Since(start))
	})
}
