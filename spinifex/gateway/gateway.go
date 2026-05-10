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
	"github.com/mulgadc/predastore/ratelimit"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gateway/policy"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// contextKey is a typed key for storing values in request context.
type contextKey string

const (
	ctxIdentity  contextKey = "sigv4.identity"
	ctxAccountID contextKey = "sigv4.accountId"
	ctxService   contextKey = "sigv4.service"
	ctxRegion    contextKey = "sigv4.region"
	ctxAccessKey contextKey = "sigv4.accessKey"
	ctxAction    contextKey = "sigv4.action"
	ctxQueryArgs contextKey = "sigv4.queryArgs"
)

type GatewayConfig struct {
	Debug          bool       `json:"debug"`
	DisableLogging bool       `json:"disable_logging"`
	NATSConn       *nats.Conn // Shared NATS connection for service communication
	Config         string     // Shared AWS Gateway config for S3 auth
	ExpectedNodes  int        // Number of expected spinifex nodes for multi-node operations
	Region         string     // Region this gateway is running in
	AZ             string     // Availability zone this gateway is running in
	IAMService     handlers_iam.IAMService
	RateLimiter    *AuthRateLimiter     // Per-IP auth failure rate limiter
	Throttler      *ratelimit.Throttler // Per-account+action API request throttler
	Version        string               // Build-time version string (set from cmd.Version)
	Commit         string               // Build-time commit hash (set from cmd.Commit)
}

var supportedServices = map[string]bool{
	"ec2":                  true,
	"iam":                  true,
	"account":              true,
	"elasticloadbalancing": true,
	"spinifex":             true,
}

const xmlnsEC2 = "http://ec2.amazonaws.com/doc/2016-11-15/"

type ErrorResponse struct {
	XMLName   xml.Name `xml:"http://ec2.amazonaws.com/doc/2016-11-15/ ErrorResponse"`
	Errors    Errors   `xml:"Errors"`
	RequestID string   `xml:"RequestID"`
}

type Errors struct {
	Error ErrorDetail `xml:"Error"`
}

type ErrorDetail struct {
	Code    string `xml:"Code"`
	Message error  `xml:"Message"`
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

	// Create a new logger with the custom handler
	slogger := slog.New(handler)

	// Set it as the default logger
	slog.SetDefault(slogger)

	// Initialize auth rate limiter if not already set.
	if gw.RateLimiter == nil {
		gw.RateLimiter = NewAuthRateLimiter()
	}

	r := chi.NewRouter()

	if !gw.DisableLogging {
		r.Use(slogRequestLogger)
	}

	// AWS SigV4 authentication middleware
	r.Use(gw.SigV4AuthMiddleware())

	// API request throttling (post-auth, per-account+action token bucket)
	if gw.Throttler != nil {
		r.Use(gw.Throttler.Middleware(
			gw.throttleKeyFuncs(),
			gw.writeThrottleError,
		))
	}

	// Catch-all routes
	r.HandleFunc("/*", gw.Request)

	return r
}

// throttleKeyFuncs returns the KeyFunc slice for the API throttle middleware.
// The first func extracts the account-id (set by SigV4 auth), the second
// extracts the action from context (set by SigV4 auth from the request body).
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

// clusterUnavailableMsg is the body returned when the gateway short-circuits
// because the daemon's NATS connection is down. Points operators at the
// daemon's /local/status (1b) for triage instead of letting them watch the
// AWS CLI hang on per-call timeouts.
const clusterUnavailableMsg = "cluster unavailable: NATS disconnected — check daemon /local/status"

// writeClusterUnavailable returns a 503 ServiceUnavailable for the given
// service-flavoured XML format. The body carries the literal
// clusterUnavailableMsg in <Message> — the generic GenerateEC2ErrorResponse
// path drops the message string (see ErrorHandler), so we emit XML directly
// here to make sure the /local/status hint actually reaches the operator.
func (gw *GatewayConfig) writeClusterUnavailable(w http.ResponseWriter, _ *http.Request, svc string) {
	requestID := uuid.NewString()
	var xmlBody string
	if svc == "iam" {
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
		// ec2, elasticloadbalancing, account, spinifex — all share the EC2 envelope.
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
	if svc == "iam" {
		errorCode = awserrors.ErrorThrottling
	}
	errorMsg := awserrors.ErrorLookup[errorCode]

	var xmlErr []byte
	if svc == "iam" {
		xmlErr = GenerateIAMErrorResponse(errorCode, errorMsg.Message, requestID)
	} else { // ec2, elasticloadbalancing, account, spinifex
		xmlErr = GenerateEC2ErrorResponse(errorCode, errorMsg.Message, requestID)
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(errorMsg.HTTPCode)
	if _, err := w.Write(xmlErr); err != nil {
		slog.Error("Failed to write throttle error response", "err", err)
	}
}

// Note, custom endpoints can be configured via ENV vars to the AWS SDK/CLI tool, with individual endpoints depending the service
// AWS_ENDPOINT_URL_EC2=https://localhost:9999/ aws  --no-verify-ssl ec2 describe-instances
// aws --endpoint-url https://localhost:9999/  --no-verify-ssl eks list-clusters
// AWS_ENDPOINT_URL=https://localhost:9999/ aws  --no-verify-ssl ec2 describe-instances

func (gw *GatewayConfig) Request(w http.ResponseWriter, r *http.Request) {
	// Route the request to the appropriate endpoint (e.g EC2, IAM, etc)
	svc, err := gw.GetService(r)
	slog.Info("Request", "service", svc, "method", r.Method, "path", r.URL.Path)

	if err != nil {
		slog.Error("GetService error", "error", err)
		gw.ErrorHandler(w, r, err)
		return
	}

	// Fail fast when NATS is down — every NATS-bound per-service handler would
	// otherwise hang until per-call timeout. The body points operators at
	// /local/status (1b) for diagnosis. Account is a no-op stub that never
	// reaches NATS, so it is exempt.
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
	case "elasticloadbalancing":
		err = gw.ELBv2_Request(w, r)
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

// checkPolicy evaluates IAM policies for the current request. Returns nil
// if access is allowed, or an ErrorAccessDenied error if denied.
// Root users bypass evaluation entirely. If the IAM service is unavailable,
// access is allowed (pre-IAM compatibility).
func (gw *GatewayConfig) checkPolicy(r *http.Request, service, action string) error {
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
	// Extract account ID from auth context
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("checkPolicy: no account ID in auth context", "user", identity)
		return errors.New(awserrors.ErrorInternalError)
	}

	if identity == "" || (identity == "root" && accountID == utils.GlobalAccountID) {
		return nil
	}

	// Resolve the IAM action string (e.g. "ec2:RunInstances")
	iamAction := policy.IAMAction(service, action)

	// Retry on transient NATS errors (e.g. during node failure / leader election).
	var policies []handlers_iam.PolicyDocument
	var err error
	for attempt := range 3 {
		policies, err = gw.IAMService.GetUserPolicies(accountID, identity)
		if err == nil || !isNATSTransient(err) {
			break
		}
		if attempt < 2 {
			slog.Debug("checkPolicy: transient NATS error, retrying",
				"user", identity, "attempt", attempt+1, "err", err)
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
		}
	}
	if err != nil {
		slog.Error("checkPolicy: failed to get user policies", "user", identity, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}

	if policy.EvaluateAccess(identity, iamAction, "*", policies) == policy.Deny {
		slog.Info("checkPolicy: access denied", "user", identity, "action", iamAction)
		return errors.New(awserrors.ErrorAccessDenied)
	}

	return nil
}

func (gw *GatewayConfig) ErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	svc, _ := gw.GetService(r)
	slog.Debug("ErrorHandler", "service", svc, "error", err.Error())

	// Generate a server-side request ID — never trust client-provided values
	var requestId = uuid.NewString()

	var errorMsg = awserrors.ErrorMessage{}

	// Check if the error lookup exists
	if _, exists := awserrors.ErrorLookup[err.Error()]; !exists {
		slog.Warn("Unknown error code", "error", err.Error())
		err = errors.New(awserrors.ErrorInternalError)
	}

	errorMsg = awserrors.ErrorLookup[err.Error()]

	// IAM uses a different error XML format than EC2
	var xmlError []byte
	if svc == "iam" {
		xmlError = GenerateIAMErrorResponse(err.Error(), errorMsg.Message, requestId)
	} else {
		xmlError = GenerateEC2ErrorResponse(err.Error(), errorMsg.Message, requestId)
	}

	slog.Debug("Generated error response", "error", err.Error(), "xml", string(xmlError), "requestId", requestId)

	if errorMsg.HTTPCode == 0 {
		errorMsg.HTTPCode = 500
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(errorMsg.HTTPCode)
	if _, err := w.Write(xmlError); err != nil {
		slog.Error("Failed to write error response", "err", err)
	}
}

// readQueryArgs returns parsed query args from context (set by SigV4) or
// parses the body. The fallback only fires for unauthenticated/test paths.
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
	errorXml := ErrorResponse{
		Errors: Errors{
			Error: ErrorDetail{
				Code:    code,
				Message: errors.New(message),
			},
		},
		RequestID: requestID,
	}

	output, err := xml.MarshalIndent(errorXml, "", "  ")

	if err != nil {
		slog.Error("Failed to build XML", "error", err)
		return []byte(xml.Header + `<ErrorResponse xmlns="` + xmlnsEC2 + `"><Errors><Error><Code>InternalError</Code><Message>Internal error</Message></Error></Errors><RequestID>` + requestID + `</RequestID></ErrorResponse>`)
	}

	// Add XML header
	output = append([]byte(xml.Header), output...)

	return output
}

// IAMErrorResponse represents the IAM-style error XML format:
// <ErrorResponse><Error><Type>Sender</Type><Code>...</Code><Message>...</Message></Error><RequestId>...</RequestId></ErrorResponse>
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

	// Create an inbox for collecting responses from all nodes
	inbox := nats.NewInbox()
	sub, err := gw.NATSConn.SubscribeSync(inbox)
	if err != nil {
		slog.Error("DiscoverActiveNodes: Failed to create inbox subscription", "err", err)
		return gw.ExpectedNodes
	}
	defer sub.Unsubscribe()

	// Publish discovery request to all nodes
	err = gw.NATSConn.PublishRequest("spinifex.nodes.discover", inbox, []byte("{}"))
	if err != nil {
		slog.Error("DiscoverActiveNodes: Failed to publish request", "err", err)
		return gw.ExpectedNodes
	}

	// Collect responses with a short timeout
	// We use a short timeout since discovery should be fast
	timeout := 500 * time.Millisecond
	deadline := time.Now().Add(timeout)

	nodesSeen := make(map[string]bool)

	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		msg, err := sub.NextMsg(remaining)
		if err != nil {
			if err == nats.ErrTimeout {
				break
			}
			slog.Debug("DiscoverActiveNodes: Error receiving message", "err", err)
			break
		}

		var response types.NodeDiscoverResponse
		if err := json.Unmarshal(msg.Data, &response); err != nil {
			slog.Debug("DiscoverActiveNodes: Failed to unmarshal response", "err", err)
			continue
		}

		nodesSeen[response.Node] = true
	}

	activeNodes := len(nodesSeen)
	if activeNodes == 0 {
		// Fallback to configured value if no responses
		slog.Warn("DiscoverActiveNodes: No nodes responded, using ExpectedNodes fallback", "fallback", gw.ExpectedNodes)
		return gw.ExpectedNodes
	}

	slog.Debug("DiscoverActiveNodes: Discovered active nodes", "count", activeNodes)
	return activeNodes
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter

	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// SlogRequestLogger is a middleware that logs each request using slog.
func slogRequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", ww.status, "duration", time.Since(start))
	})
}
