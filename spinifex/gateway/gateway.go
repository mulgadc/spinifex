package gateway

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/predastore/ratelimit"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
	"github.com/mulgadc/spinifex/spinifex/gateway/policy"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
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
)

// Values stored under ctxPrincipalType. Downstream handlers that interpret
// ctxIdentity as an IAM user name MUST gate on principalTypeUser; otherwise a
// session whose SessionName collides with a user name inherits that user's policies.
const (
	principalTypeUser        = "user"
	principalTypeAssumedRole = "assumed-role"
	principalTypeRoot        = "root"
)

type GatewayConfig struct {
	Debug          bool       `json:"debug"`
	DisableLogging bool       `json:"disable_logging"`
	NATSConn       *nats.Conn // Shared NATS connection for service communication
	Config         string     // Shared AWS Gateway config for S3 auth
	ExpectedNodes  int        // Number of expected spinifex nodes for multi-node operations
	Region         string     // Region this gateway is running in
	InternalSuffix string     // Internal DNS suffix for AWS-parity endpoints (e.g. spinifex.internal)
	AZ             string     // Availability zone this gateway is running in
	IAMService     handlers_iam.IAMService
	STSService     handlers_sts.STSService
	RateLimiter    *AuthRateLimiter     // Per-IP auth failure rate limiter
	Throttler      *ratelimit.Throttler // Per-account+action API request throttler
	Version        string               // Build-time version string (set from cmd.Version)
	Commit         string               // Build-time commit hash (set from cmd.Commit)
	// ECRRegistry serves the OCI Distribution v2 (/v2/*) surface. Nil falls back
	// to the 501 stub (e.g. in unit tests of unrelated routes).
	ECRRegistry *gateway_ecr.Registry
	// ECRTokenIssuer mints GetAuthorizationToken JWTs; ECRTokenVerifier validates
	// them on /v2/*. Both nil disables the auth bridge (registry mounts open, as
	// in unit tests of unrelated routes).
	ECRTokenIssuer   *gateway_ecrauth.Issuer
	ECRTokenVerifier *gateway_ecrauth.Verifier
}

var supportedServices = map[string]bool{
	"ec2":                  true,
	"iam":                  true,
	"sts":                  true,
	"account":              true,
	"elasticloadbalancing": true,
	"eks":                  true,
	"ecr":                  true,
	"acm":                  true,
	"tagging":              true,
	"spinifex":             true,
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

func (gw *GatewayConfig) SetupRoutes() http.Handler {
	var logLevel slog.Level

	if gw.Debug {
		logLevel = slog.LevelDebug
	} else if gw.DisableLogging {
		logLevel = slog.LevelError
	} else {
		logLevel = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})

	slogger := slog.New(handler)
	slog.SetDefault(slogger)

	if gw.RateLimiter == nil {
		gw.RateLimiter = NewAuthRateLimiter()
	}

	r := chi.NewRouter()

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

		// Post-auth, per-account+action token bucket throttle.
		if gw.Throttler != nil {
			auth.Use(gw.Throttler.Middleware(
				gw.throttleKeyFuncs(),
				gw.writeThrottleError,
			))
		}

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

// clusterUnavailableMsg is the 503 body when NATS is disconnected. Points
// operators at /local/status rather than leaving the AWS CLI hanging on timeouts.
const clusterUnavailableMsg = "cluster unavailable: NATS disconnected — check daemon /local/status"

// writeClusterUnavailable writes a 503 ServiceUnavailable in the service-appropriate
// format. It emits XML directly (not via GenerateEC2ErrorResponse) to ensure the
// /local/status hint is preserved in <Message>.
func (gw *GatewayConfig) writeClusterUnavailable(w http.ResponseWriter, _ *http.Request, svc string) {
	requestID := uuid.NewString()

	// EKS uses AWS REST-JSON 1.1.
	if svc == "eks" {
		body := GenerateEKSErrorResponse(awserrors.ErrorServiceUnavailable, clusterUnavailableMsg, requestID)
		w.Header().Set("Content-Type", eksJSONContentType)
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := w.Write(body); err != nil {
			slog.Error("Failed to write EKS cluster-unavailable response", "err", err)
		}
		return
	}

	var xmlBody string
	if svc == "iam" || svc == "sts" {
		iam := IAMErrorResponse{
			Error: IAMErrorDetail{
				Type:    "Sender",
				Code:    awserrors.ErrorServiceUnavailable,
				Message: clusterUnavailableMsg,
			},
			RequestID: requestID,
		}
		out, err := xml.MarshalIndent(iam, "", "  ")
		if err != nil {
			slog.Error("Failed to marshal IAM cluster-unavailable XML", "err", err)
			out = []byte(`<ErrorResponse><Error><Type>Sender</Type><Code>ServiceUnavailable</Code><Message>` + clusterUnavailableMsg + `</Message></Error><RequestId>` + requestID + `</RequestId></ErrorResponse>`)
		}
		xmlBody = xml.Header + string(out)
	} else {
		// ec2, elasticloadbalancing, account, spinifex all share the EC2 envelope.
		xmlBody = xml.Header + `<Response><Errors><Error><Code>` + awserrors.ErrorServiceUnavailable +
			`</Code><Message>` + clusterUnavailableMsg + `</Message></Error></Errors><RequestID>` +
			requestID + `</RequestID></Response>`
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusServiceUnavailable)
	if _, err := w.Write([]byte(xmlBody)); err != nil {
		slog.Error("Failed to write cluster-unavailable response", "err", err)
	}
}

// writeThrottleError writes the service-appropriate throttle rejection response.
func (gw *GatewayConfig) writeThrottleError(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	svc, _ := r.Context().Value(ctxService).(string)

	errorCode := awserrors.ErrorRequestLimitExceeded
	if svc == "iam" || svc == "sts" || svc == "eks" {
		errorCode = awserrors.ErrorThrottling
	}
	errorMsg := awserrors.ErrorLookup[errorCode]

	// EKS uses AWS REST-JSON 1.1.
	if svc == "eks" {
		body := GenerateEKSErrorResponse(errorCode, errorMsg.Message, requestID)
		w.Header().Set("Content-Type", eksJSONContentType)
		w.WriteHeader(errorMsg.HTTPCode)
		if _, err := w.Write(body); err != nil {
			slog.Error("Failed to write EKS throttle error response", "err", err)
		}
		return
	}

	var xmlErr []byte
	if svc == "iam" || svc == "sts" || svc == "elasticloadbalancing" {
		xmlErr = GenerateIAMErrorResponse(errorCode, errorMsg.Message, requestID)
	} else { // ec2, account, spinifex
		xmlErr = GenerateEC2ErrorResponse(errorCode, errorMsg.Message, requestID)
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(errorMsg.HTTPCode)
	if _, err := w.Write(xmlErr); err != nil {
		slog.Error("Failed to write throttle error response", "err", err)
	}
}

func (gw *GatewayConfig) Request(w http.ResponseWriter, r *http.Request) {
	svc, err := gw.GetService(r)
	slog.Info("Request", "service", svc, "method", r.Method, "path", r.URL.Path)

	if err != nil {
		slog.Error("GetService error", "error", err)
		gw.ErrorHandler(w, r, err)
		return
	}

	// Fail fast when NATS is down; every NATS-bound handler would otherwise hang
	// until per-call timeout. Account is a local stub exempt from NATS.
	if svc != "account" && (gw.NATSConn == nil || !gw.NATSConn.IsConnected()) {
		gw.writeClusterUnavailable(w, r, svc)
		return
	}

	switch svc {
	case "ec2":
		err = gw.EC2_Request(w, r)
	case "account":
		err = gw.Account_Request(w, r)
	case "iam":
		err = gw.IAM_Request(w, r)
	case "sts":
		err = gw.STS_Request(w, r)
	case "elasticloadbalancing":
		err = gw.ELBv2_Request(w, r)
	case "eks":
		err = gw.EKS_Request(w, r)
	case "ecr":
		err = gw.ECR_Request(w, r)
	case "acm":
		err = gw.ACM_Request(w, r)
	case "tagging":
		err = gw.Tagging_Request(w, r)
	case "spinifex":
		err = gw.Spinifex_Request(w, r)
	default:
		err = errors.New(awserrors.ErrorUnsupportedOperation)
	}

	if err != nil {
		slog.Error("Service request error", "service", svc, "error", err)
		gw.ErrorHandler(w, r, err)
	} else {
		slog.Info("Service request completed", "service", svc)
	}
}

func (gw *GatewayConfig) GetService(r *http.Request) (string, error) {
	svc, ok := r.Context().Value(ctxService).(string)
	if !ok {
		return "", errors.New(awserrors.ErrorAuthFailure)
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

// checkPolicy evaluates IAM policies against resource "*".
// Shorthand for checkPolicyResource(r, service, action, "*").
func (gw *GatewayConfig) checkPolicy(r *http.Request, service, action string) error {
	return gw.checkPolicyResource(r, service, action, "*")
}

// checkPolicyResource evaluates IAM policies against a specific resource ARN.
// Root users bypass evaluation. Nil IAMService allows (pre-IAM compatibility).
// Used by EC2 paths that enforce iam:PassRole before attaching an instance profile.
func (gw *GatewayConfig) checkPolicyResource(r *http.Request, service, action, resource string) error {
	if gw.IAMService == nil {
		slog.Warn("checkPolicy: IAM service not available, skipping policy check",
			"service", service, "action", action)
		return nil
	}

	identityVal := r.Context().Value(ctxIdentity)
	if identityVal == nil {
		// No auth context — pre-IAM compatibility
		return nil
	}
	identity, ok := identityVal.(string)
	if !ok {
		slog.Error("checkPolicy: identity has unexpected type", "type", fmt.Sprintf("%T", identityVal))
		return errors.New(awserrors.ErrorInternalError)
	}
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("checkPolicy: no account ID in auth context", "user", identity)
		return errors.New(awserrors.ErrorInternalError)
	}

	iamAction := policy.IAMAction(service, action)

	// Each branch resolves the policy resolver and log identity for its principal
	// type. Identity-sensitive decisions (root bypass, resolver selection) are
	// fully inside their branch so an assumed-role SessionName of "root" cannot
	// reach the user-branch root short-circuit.
	var resolve func() ([]handlers_iam.PolicyDocument, error)
	var logIdentity string

	principalType, _ := r.Context().Value(ctxPrincipalType).(string)
	switch principalType {
	case principalTypeUser:
		if identity == "" || (identity == "root" && accountID == utils.GlobalAccountID) {
			// root / pre-IAM bypass — user branch only.
			return nil
		}
		resolve = func() ([]handlers_iam.PolicyDocument, error) {
			return gw.IAMService.GetUserPolicies(accountID, identity)
		}
		logIdentity = identity
	case principalTypeAssumedRole:
		// Resolve by the session's underlying role, never by SessionName (attacker-influenced).
		// A missing/legacy, cross-account, or malformed ARN fails closed with AccessDenied.
		underlyingRoleARN, _ := r.Context().Value(ctxUnderlyingRoleARN).(string)
		roleAcct, roleName, perr := auth.ParseRoleARN(underlyingRoleARN)
		if perr != nil || roleAcct != accountID {
			slog.Warn("checkPolicy: unresolvable or cross-account assumed-role principal denied",
				"underlyingRoleARN", underlyingRoleARN,
				"accountID", accountID,
				"action", iamAction,
				"err", perr)
			return errors.New(awserrors.ErrorAccessDenied)
		}
		resolve = func() ([]handlers_iam.PolicyDocument, error) {
			return gw.IAMService.GetRolePolicies(accountID, roleName)
		}
		logIdentity, _ = r.Context().Value(ctxAssumedRoleARN).(string)
	default:
		slog.Error("checkPolicy: unknown principal type", "principalType", principalType)
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
			slog.Debug("checkPolicy: transient NATS error, retrying",
				"identity", logIdentity, "attempt", attempt+1, "err", err)
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
		}
	}
	if err != nil {
		slog.Error("checkPolicy: failed to resolve policies", "identity", logIdentity, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}

	if policy.EvaluateAccess(logIdentity, iamAction, resource, policies) == policy.Deny {
		slog.Info("checkPolicy: access denied", "identity", logIdentity, "action", iamAction, "resource", resource)
		return errors.New(awserrors.ErrorAccessDenied)
	}

	return nil
}

func (gw *GatewayConfig) ErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	svc, _ := gw.GetService(r)
	slog.Debug("ErrorHandler", "service", svc, "error", err.Error())

	var requestId = uuid.NewString()
	var errorMsg = awserrors.ErrorMessage{}

	if _, exists := awserrors.ErrorLookup[err.Error()]; !exists {
		slog.Warn("Unknown error code", "error", err.Error())
		err = errors.New(awserrors.ErrorInternalError)
	}

	errorMsg = awserrors.ErrorLookup[err.Error()]

	if errorMsg.HTTPCode == 0 {
		errorMsg.HTTPCode = 500
	}

	// EKS, ECR, ACM, and tagging use AWS JSON 1.1; query/XML services fall through.
	if svc == "eks" || svc == "ecr" || svc == "acm" || svc == "tagging" {
		body := GenerateEKSErrorResponse(err.Error(), errorMsg.Message, requestId)
		slog.Debug("Generated JSON error response", "service", svc, "error", err.Error(), "json", string(body), "requestId", requestId)
		w.Header().Set("Content-Type", eksJSONContentType)
		w.WriteHeader(errorMsg.HTTPCode)
		if _, err := w.Write(body); err != nil {
			slog.Error("Failed to write EKS error response", "err", err)
		}
		return
	}

	var xmlError []byte
	if svc == "iam" || svc == "sts" || svc == "elasticloadbalancing" {
		xmlError = GenerateIAMErrorResponse(err.Error(), errorMsg.Message, requestId)
	} else {
		xmlError = GenerateEC2ErrorResponse(err.Error(), errorMsg.Message, requestId)
	}

	slog.Debug("Generated error response", "error", err.Error(), "xml", string(xmlError), "requestId", requestId)

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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return ParseAWSQueryArgs(string(body))
}

// ParseAWSQueryArgs parses an AWS query-protocol body. Returns an error on
// invalid percent-encoding so callers can surface MalformedQueryString.
func ParseAWSQueryArgs(query string) (map[string]string, error) {
	params := make(map[string]string)
	pairs := strings.SplitSeq(query, "&")
	for pair := range pairs {
		kv := strings.SplitN(pair, "=", 2)
		key, err := url.QueryUnescape(kv[0])
		if err != nil {
			return nil, fmt.Errorf("invalid URL encoding in parameter name: %w", err)
		}
		if len(kv) == 2 {
			value, err := url.QueryUnescape(kv[1])
			if err != nil {
				return nil, fmt.Errorf("invalid URL encoding in value for %q: %w", key, err)
			}
			params[key] = value
		} else {
			params[key] = ""
		}
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

func ParseArgsToStruct(input *any, args map[string]string) (err error) {
	// Generated from input shape: RunInstancesRequest
	err = awsec2query.QueryParamsToStruct(args, input)

	if err != nil {
		return errors.New(awserrors.ErrorInvalidParameter)
	}

	return nil
}

// DiscoverActiveNodes discovers the number of active spinifex daemon nodes in the cluster
// by publishing a discovery request and counting unique responses.
// Returns the number of active nodes (minimum 1 if fallback is needed).
func (gw *GatewayConfig) DiscoverActiveNodes() int {
	if gw.NATSConn == nil {
		slog.Warn("DiscoverActiveNodes: NATS connection not available, using ExpectedNodes fallback", "fallback", gw.ExpectedNodes)
		return gw.ExpectedNodes
	}

	frames, _, err := utils.Gather(gw.NATSConn, "spinifex.nodes.discover", []byte("{}"),
		utils.GatherOpts{Timeout: 500 * time.Millisecond})
	if err != nil {
		slog.Error("DiscoverActiveNodes: fan-out failed, using ExpectedNodes fallback", "err", err, "fallback", gw.ExpectedNodes)
		return gw.ExpectedNodes
	}

	nodesSeen := make(map[string]bool)
	for _, frame := range frames {
		var response types.NodeDiscoverResponse
		if err := json.Unmarshal(frame, &response); err != nil {
			slog.Debug("DiscoverActiveNodes: Failed to unmarshal response", "err", err)
			continue
		}
		nodesSeen[response.Node] = true
	}

	activeNodes := len(nodesSeen)
	if activeNodes == 0 {
		slog.Warn("DiscoverActiveNodes: No nodes responded, using ExpectedNodes fallback", "fallback", gw.ExpectedNodes)
		return gw.ExpectedNodes
	}

	slog.Debug("DiscoverActiveNodes: Discovered active nodes", "count", activeNodes)
	return activeNodes
}

// statusWriter wraps http.ResponseWriter to capture the written status code.
type statusWriter struct {
	http.ResponseWriter

	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// slogRequestLogger is a middleware that logs each request via slog.
func slogRequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", ww.status, "duration", time.Since(start))
	})
}
