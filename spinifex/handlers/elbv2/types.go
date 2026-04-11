package handlers_elbv2

import "time"

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

	// Listener action types
	ActionTypeForward = "forward"

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
	ConfigHash      string            `json:"config_hash,omitempty"` // SHA256 of ConfigText
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
	TargetType     string            `json:"target_type"` // "instance" for v1
	HealthCheck    HealthCheckConfig `json:"health_check"`
	Targets        []Target          `json:"targets"`
	Attributes     map[string]string `json:"attributes,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	AccountID      string            `json:"account_id"`
	CreatedAt      time.Time         `json:"created_at"`
}

// HealthCheckConfig defines health check parameters for a target group.
type HealthCheckConfig struct {
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
	Id          string `json:"id"`           // Instance ID (e.g. i-xxxxx)
	Port        int64  `json:"port"`         // Override port (0 = use TG default)
	HealthState string `json:"health_state"` // "initial", "healthy", "unhealthy", "draining"
	HealthDesc  string `json:"health_desc"`  // Reason for current state
	PrivateIP   string `json:"private_ip"`   // Resolved from instance ENI
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
}

// ListenerAction defines a listener's default action.
type ListenerAction struct {
	Type           string `json:"type"` // "forward"
	TargetGroupArn string `json:"target_group_arn"`
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
	// Common to all load balancer types.
	attrs := map[string]string{
		"deletion_protection.enabled":       "false",
		"load_balancing.cross_zone.enabled": "false",
	}

	switch lbType {
	case LoadBalancerTypeApplication:
		// ALB-specific attributes. Defaults match real AWS values as of the
		// aws-sdk-go 1.55 elbv2 API documentation.
		attrs["load_balancing.cross_zone.enabled"] = "true"
		attrs["access_logs.s3.enabled"] = "false"
		attrs["access_logs.s3.bucket"] = ""
		attrs["access_logs.s3.prefix"] = ""
		attrs["connection_logs.s3.enabled"] = "false"
		attrs["connection_logs.s3.bucket"] = ""
		attrs["connection_logs.s3.prefix"] = ""
		attrs["idle_timeout.timeout_seconds"] = "60"
		attrs["client_keep_alive.seconds"] = "3600"
		attrs["routing.http.desync_mitigation_mode"] = "defensive"
		attrs["routing.http.drop_invalid_header_fields.enabled"] = "false"
		attrs["routing.http.preserve_host_header.enabled"] = "false"
		attrs["routing.http.x_amzn_tls_version_and_cipher_suite.enabled"] = "false"
		attrs["routing.http.xff_client_port.enabled"] = "false"
		attrs["routing.http.xff_header_processing.mode"] = "append"
		attrs["routing.http2.enabled"] = "true"
		attrs["waf.fail_open.enabled"] = "false"
		attrs["zonal_shift.config.enabled"] = "false"
	case LoadBalancerTypeNetwork:
		// NLB-specific attributes.
		attrs["access_logs.s3.enabled"] = "false"
		attrs["access_logs.s3.bucket"] = ""
		attrs["access_logs.s3.prefix"] = ""
		attrs["dns_record.client_routing_policy"] = "any_availability_zone"
		attrs["ipv6.deny_all_igw_traffic"] = "false"
		attrs["zonal_shift.config.enabled"] = "false"
	}

	return attrs
}

// DefaultTargetGroupAttributes returns the default attribute set for target groups.
func DefaultTargetGroupAttributes() map[string]string {
	return map[string]string{
		"deregistration_delay.timeout_seconds":  "300",
		"stickiness.enabled":                    "false",
		"stickiness.type":                       "lb_cookie",
		"stickiness.lb_cookie.duration_seconds": "86400",
		"load_balancing.cross_zone.enabled":     "use_load_balancer_configuration",
		"load_balancing.algorithm.type":         "round_robin",
		"slow_start.duration_seconds":           "0",
	}
}
