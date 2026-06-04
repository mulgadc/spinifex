package handlers_elbv2

import (
	"maps"
	"time"
)

const (
	// LoadBalancer types
	LoadBalancerTypeApplication = "application"
	LoadBalancerTypeNetwork     = "network"

	// LoadBalancer schemes
	SchemeInternetFacing = "internet-facing"
	SchemeInternal       = "internal"

	// LoadBalancer states
	StateProvisioning = "provisioning"
	StateActive       = "active"
	StateFailed       = "failed"

	// Target group target types (v1 supports instance and ip)
	TargetTypeInstance = "instance"
	TargetTypeIP       = "ip"

	// Target health states
	TargetHealthInitial   = "initial"
	TargetHealthHealthy   = "healthy"
	TargetHealthUnhealthy = "unhealthy"
	TargetHealthDraining  = "draining"
	TargetHealthUnused    = "unused"

	// Listener protocols (ALB)
	ProtocolHTTP  = "HTTP"
	ProtocolHTTPS = "HTTPS"

	// Listener protocols (NLB)
	ProtocolTCP    = "TCP"
	ProtocolUDP    = "UDP"
	ProtocolTLS    = "TLS"
	ProtocolTCPUDP = "TCP_UDP"

	// Data-plane engines. ALBs (L7) run HAProxy; NLBs (L4) run nginx `stream`
	// because HAProxy cannot load-balance UDP. The agent learns which engine to
	// run from the GetLBConfig response.
	EngineHAProxy = "haproxy"
	EngineNginx   = "nginx"

	// DefaultSslPolicy is applied to a secure listener when the caller does
	// not specify an SslPolicy.
	DefaultSslPolicy = "ELBSecurityPolicy-2016-08"

	// Listener action types
	ActionTypeForward       = "forward"
	ActionTypeFixedResponse = "fixed-response"
	ActionTypeRedirect      = "redirect"

	// Rule condition fields
	RuleFieldHostHeader        = "host-header"
	RuleFieldPathPattern       = "path-pattern"
	RuleFieldHTTPHeader        = "http-header"
	RuleFieldHTTPRequestMethod = "http-request-method"
	RuleFieldQueryString       = "query-string"
	RuleFieldSourceIP          = "source-ip"

	// Rule priority bounds (AWS-compatible).
	RuleMinPriority = 1
	RuleMaxPriority = 50000

	// Rule limits per listener.
	MaxRulesPerListener   = 100
	MaxConditionsPerRule  = 5
	MaxValuesPerCondition = 5
	MaxActionsPerRule     = 1 // single terminal action: forward, redirect, or fixed-response
	MaxConditionValueLen  = 128
	MaxHTTPHeaderNameLen  = 40

	// Default health check values (ALB)
	DefaultHealthCheckInterval           = 30
	DefaultHealthCheckTimeout            = 5
	DefaultHealthyThreshold              = 5
	DefaultUnhealthyThreshold            = 2
	DefaultHealthCheckPath               = "/"
	DefaultHealthCheckPort               = "traffic-port"
	DefaultHealthCheckProtocol           = ProtocolHTTP
	DefaultHealthCheckMatcher            = "200"
	DefaultTargetDeregistrationDelaySecs = 300

	// Default health check values (NLB)
	DefaultNLBHealthCheckInterval = 30
	DefaultNLBHealthCheckTimeout  = 10
	DefaultNLBHealthyThreshold    = 3
	DefaultNLBUnhealthyThreshold  = 3
	DefaultNLBHealthCheckProtocol = ProtocolTCP
	DefaultNLBHealthCheckPort     = "traffic-port"

	// IP address type
	IPAddressTypeIPv4 = "ipv4"
)

// LoadBalancerRecord represents a stored load balancer (ALB or NLB).
type LoadBalancerRecord struct {
	LoadBalancerArn string            `json:"load_balancer_arn"`
	LoadBalancerID  string            `json:"load_balancer_id"` // Short ID (hex suffix)
	DNSName         string            `json:"dns_name"`
	Name            string            `json:"name"`
	Scheme          string            `json:"scheme"` // "internet-facing" or "internal"
	Type            string            `json:"type"`   // Always "application"
	State           string            `json:"state"`  // "provisioning", "active", "failed"
	VpcId           string            `json:"vpc_id"`
	SecurityGroups  []string          `json:"security_groups"`
	Subnets         []string          `json:"subnets"`
	AvailZones      []AvailZoneInfo   `json:"availability_zones"`
	ENIs            []string          `json:"enis,omitempty"`        // ENI IDs created for this ALB (internal)
	InstanceID      string            `json:"instance_id,omitempty"` // ALB VM instance ID (system-managed)
	VPCIP           string            `json:"vpc_ip,omitempty"`      // VPC private IP of the ALB VM
	ConfigText      string            `json:"config_text,omitempty"` // Pre-computed HAProxy config
	ConfigHash      string            `json:"config_hash,omitempty"` // SHA256 of ConfigText + cert material
	CertFiles       map[string]string `json:"cert_files,omitempty"`  // Absolute path → combined PEM (cert+chain+key), delivered with config
	LastHeartbeat   time.Time         `json:"last_heartbeat"`        // Last agent heartbeat timestamp
	HostPorts       map[int]int       `json:"host_ports,omitempty"`  // Dev mode: guest port → host port forwarding
	NodeID          string            `json:"node_id"`               // Daemon node running this ALB
	IPAddressType   string            `json:"ip_address_type"`       // "ipv4"
	Attributes      map[string]string `json:"attributes,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
	AccountID       string            `json:"account_id"`
	CreatedAt       time.Time         `json:"created_at"`
}

// AvailZoneInfo tracks subnet-to-AZ mapping for a load balancer.
type AvailZoneInfo struct {
	ZoneName string `json:"zone_name"`
	SubnetId string `json:"subnet_id"`
	PublicIP string `json:"public_ip,omitempty"` // Set for internet-facing ALBs
}

// TargetGroupRecord represents a stored Target Group.
type TargetGroupRecord struct {
	TargetGroupArn string            `json:"target_group_arn"`
	TargetGroupID  string            `json:"target_group_id"` // Short ID (hex suffix)
	Name           string            `json:"name"`
	Protocol       string            `json:"protocol"` // "HTTP" or "HTTPS"
	Port           int64             `json:"port"`     // Default target port
	VpcId          string            `json:"vpc_id"`
	TargetType     string            `json:"target_type"` // TargetTypeInstance or TargetTypeIP
	HealthCheck    HealthCheckConfig `json:"health_check"`
	Targets        []Target          `json:"targets"`
	Attributes     map[string]string `json:"attributes,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	AccountID      string            `json:"account_id"`
	CreatedAt      time.Time         `json:"created_at"`
}

// HealthCheckConfig defines health check parameters for a target group.
type HealthCheckConfig struct {
	Enabled            bool   `json:"enabled"`
	Protocol           string `json:"protocol"`
	Port               string `json:"port"` // Port number or "traffic-port"
	Path               string `json:"path"`
	IntervalSeconds    int64  `json:"interval_seconds"`
	TimeoutSeconds     int64  `json:"timeout_seconds"`
	HealthyThreshold   int64  `json:"healthy_threshold"`
	UnhealthyThreshold int64  `json:"unhealthy_threshold"`
	Matcher            string `json:"matcher"` // HTTP codes e.g. "200" or "200-299"
}

// DefaultHealthCheck returns a HealthCheckConfig with ALB default values.
func DefaultHealthCheck() HealthCheckConfig {
	return HealthCheckConfig{
		Enabled:            true,
		Protocol:           DefaultHealthCheckProtocol,
		Port:               DefaultHealthCheckPort,
		Path:               DefaultHealthCheckPath,
		IntervalSeconds:    DefaultHealthCheckInterval,
		TimeoutSeconds:     DefaultHealthCheckTimeout,
		HealthyThreshold:   DefaultHealthyThreshold,
		UnhealthyThreshold: DefaultUnhealthyThreshold,
		Matcher:            DefaultHealthCheckMatcher,
	}
}

// DefaultNLBHealthCheck returns a HealthCheckConfig with NLB default values.
// NLB uses TCP health checks by default (no path or matcher).
func DefaultNLBHealthCheck() HealthCheckConfig {
	return HealthCheckConfig{
		Enabled:            true,
		Protocol:           DefaultNLBHealthCheckProtocol,
		Port:               DefaultNLBHealthCheckPort,
		IntervalSeconds:    DefaultNLBHealthCheckInterval,
		TimeoutSeconds:     DefaultNLBHealthCheckTimeout,
		HealthyThreshold:   DefaultNLBHealthyThreshold,
		UnhealthyThreshold: DefaultNLBUnhealthyThreshold,
	}
}

// Target represents a registered target in a target group.
type Target struct {
	Id          string `json:"id"`           // Instance ID (i-xxxxx) or IP for ip-type TGs
	Port        int64  `json:"port"`         // Override port (0 = use TG default)
	HealthState string `json:"health_state"` // "initial", "healthy", "unhealthy", "draining"
	HealthDesc  string `json:"health_desc"`  // Reason for current state
	PrivateIP   string `json:"private_ip"`   // Instance ENI IP, or the target IP for ip-type TGs
}

// ListenerRecord represents a stored Listener.
type ListenerRecord struct {
	ListenerArn     string           `json:"listener_arn"`
	ListenerID      string           `json:"listener_id"` // Short ID (hex suffix)
	LoadBalancerArn string           `json:"load_balancer_arn"`
	Protocol        string           `json:"protocol"` // "HTTP" or "HTTPS"
	Port            int64            `json:"port"`
	DefaultActions  []ListenerAction `json:"default_actions"`
	AccountID       string           `json:"account_id"`
	CreatedAt       time.Time        `json:"created_at"`

	// Certificates holds the listener's TLS certificates for a secure
	// protocol (ALB HTTPS / NLB TLS). The first IsDefault entry is the
	// default cert; the rest are additional SNI certs. Empty for non-secure
	// listeners. SslPolicy is the negotiated security policy name.
	Certificates []ListenerCertificate `json:"certificates,omitempty"`
	SslPolicy    string                `json:"ssl_policy,omitempty"`

	Tags map[string]string `json:"tags,omitempty"`
}

// ListenerCertificate is a TLS certificate reference attached to a listener.
type ListenerCertificate struct {
	CertificateArn string `json:"certificate_arn"`
	IsDefault      bool   `json:"is_default"`
}

// ListenerAction defines a listener default action or a rule action.
type ListenerAction struct {
	Type           string `json:"type"` // "forward", "redirect", or "fixed-response"
	TargetGroupArn string `json:"target_group_arn"`
	// FixedResponse is populated when Type == "fixed-response" (no target group).
	FixedResponse *FixedResponseAction `json:"fixed_response,omitempty"`
	// Redirect is populated when Type == "redirect".
	Redirect *RedirectAction `json:"redirect,omitempty"`
}

// FixedResponseAction holds the canned reply for a "fixed-response" action.
type FixedResponseAction struct {
	StatusCode  string `json:"status_code"`
	ContentType string `json:"content_type,omitempty"`
	MessageBody string `json:"message_body,omitempty"`
}

// RedirectAction holds the target for a "redirect" action. Fields mirror the
// AWS RedirectActionConfig and may contain the `#{protocol}`, `#{host}`,
// `#{port}`, `#{path}`, and `#{query}` placeholders.
type RedirectAction struct {
	Protocol   string `json:"protocol,omitempty"`
	Host       string `json:"host,omitempty"`
	Port       string `json:"port,omitempty"`
	Path       string `json:"path,omitempty"`
	Query      string `json:"query,omitempty"`
	StatusCode string `json:"status_code"` // "HTTP_301" or "HTTP_302"
}

// RuleRecord represents a stored listener rule.
type RuleRecord struct {
	RuleArn     string           `json:"rule_arn"`
	RuleID      string           `json:"rule_id"`
	ListenerArn string           `json:"listener_arn"`
	Priority    int              `json:"priority"`
	Conditions  []RuleCondition  `json:"conditions"`
	Actions     []ListenerAction `json:"actions"`
	AccountID   string           `json:"account_id"`
	CreatedAt   time.Time        `json:"created_at"`

	Tags map[string]string `json:"tags,omitempty"`
}

// RuleCondition is one routing predicate on a rule. Only the field block
// matching Field is populated.
type RuleCondition struct {
	Field          string              `json:"field"`
	Values         []string            `json:"values,omitempty"`
	HTTPHeaderName string              `json:"http_header_name,omitempty"`
	QueryStringKVs []RuleQueryStringKV `json:"query_string_kvs,omitempty"`
}

// RuleQueryStringKV is one query-string key/value pair for a query-string
// condition. Key is optional (empty Key matches any key with the given Value).
type RuleQueryStringKV struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value"`
}

// DefaultLoadBalancerAttributes returns the default attribute set for a load
// balancer of the given type. ALBs default load_balancing.cross_zone.enabled to
// "true"; NLBs (and any unknown type) default it to "false". This is the single
// source of truth — CreateLoadBalancer no longer seeds attributes, and
// DescribeLoadBalancerAttributes derives the per-type defaults from lb.Type on
// read.
//
// The key set must be broad enough to satisfy terraform AWS provider's
// default ModifyLoadBalancerAttributes call after aws_lb creation: the
// provider sends every attribute it knows about, and any key missing here
// gets rejected with ValidationError, surfacing as "UnknownError" in tofu.
func DefaultLoadBalancerAttributes(lbType string) map[string]string {
	switch lbType {
	case LoadBalancerTypeApplication:
		return maps.Clone(albAttributeDefaults)
	case LoadBalancerTypeNetwork:
		return maps.Clone(nlbAttributeDefaults)
	default:
		return maps.Clone(lbBaseAttributeDefaults)
	}
}

// DefaultTargetGroupAttributes returns the default attribute set for target groups.
func DefaultTargetGroupAttributes() map[string]string {
	return maps.Clone(targetGroupAttributeDefaults)
}

// Default attribute sets are package-level so Describe calls clone a prebuilt
// map instead of rebuilding it on every request. Callers receive a clone, never
// the shared map, so mutation cannot leak back into the defaults.
//
// access_logs.s3.bucket / .prefix (and the connection_logs equivalents) default
// to empty strings: that matches real AWS, where logging is disabled by default
// and the bucket/prefix are unset until the operator opts in. Terraform sends
// these empty strings on every apply, so they must be present as known keys.
var (
	// Common to all load balancer types; also the fallback for unknown types.
	lbBaseAttributeDefaults = map[string]string{
		"deletion_protection.enabled":       "false",
		"load_balancing.cross_zone.enabled": "false",
	}

	// ALB defaults match real AWS values as of the aws-sdk-go 1.55 elbv2 API.
	albAttributeDefaults = map[string]string{
		"deletion_protection.enabled":                              "false",
		"load_balancing.cross_zone.enabled":                        "true",
		"access_logs.s3.enabled":                                   "false",
		"access_logs.s3.bucket":                                    "",
		"access_logs.s3.prefix":                                    "",
		"connection_logs.s3.enabled":                               "false",
		"connection_logs.s3.bucket":                                "",
		"connection_logs.s3.prefix":                                "",
		"idle_timeout.timeout_seconds":                             "60",
		"client_keep_alive.seconds":                                "3600",
		"routing.http.desync_mitigation_mode":                      "defensive",
		"routing.http.drop_invalid_header_fields.enabled":          "false",
		"routing.http.preserve_host_header.enabled":                "false",
		"routing.http.x_amzn_tls_version_and_cipher_suite.enabled": "false",
		"routing.http.xff_client_port.enabled":                     "false",
		"routing.http.xff_header_processing.mode":                  "append",
		"routing.http2.enabled":                                    "true",
		"waf.fail_open.enabled":                                    "false",
		"zonal_shift.config.enabled":                               "false",
	}

	nlbAttributeDefaults = map[string]string{
		"deletion_protection.enabled":       "false",
		"load_balancing.cross_zone.enabled": "false",
		"access_logs.s3.enabled":            "false",
		"access_logs.s3.bucket":             "",
		"access_logs.s3.prefix":             "",
		"dns_record.client_routing_policy":  "any_availability_zone",
		"ipv6.deny_all_igw_traffic":         "false",
		"zonal_shift.config.enabled":        "false",
	}

	targetGroupAttributeDefaults = map[string]string{
		"deregistration_delay.timeout_seconds":  "300",
		"stickiness.enabled":                    "false",
		"stickiness.type":                       "lb_cookie",
		"stickiness.lb_cookie.duration_seconds": "86400",
		"load_balancing.cross_zone.enabled":     "use_load_balancer_configuration",
		"load_balancing.algorithm.type":         "round_robin",
		"slow_start.duration_seconds":           "0",
	}
)
