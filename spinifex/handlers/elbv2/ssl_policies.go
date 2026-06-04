package handlers_elbv2

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
)

// protocolRequiresCert reports whether a listener protocol terminates TLS and
// therefore requires at least one certificate: ALB HTTPS and NLB TLS.
func protocolRequiresCert(protocol string) bool {
	switch protocol {
	case ProtocolHTTPS, ProtocolTLS:
		return true
	default:
		return false
	}
}

// sslPolicyCipher is a named cipher with its negotiation priority.
type sslPolicyCipher struct {
	name     string
	priority int64
}

// sslPolicyDef is a static security-policy definition served by
// DescribeSSLPolicies. Spinifex does not terminate TLS yet, so these exist to
// satisfy listener SslPolicy validation and terraform reads.
type sslPolicyDef struct {
	name      string
	protocols []string
	ciphers   []sslPolicyCipher
}

// sslPolicyCatalog is the fixed set of supported security policies, keyed by
// name. Static data — no persistence.
var sslPolicyCatalog = map[string]sslPolicyDef{
	DefaultSslPolicy: {
		name:      DefaultSslPolicy,
		protocols: []string{"TLSv1.2"},
		ciphers: []sslPolicyCipher{
			{"ECDHE-ECDSA-AES128-GCM-SHA256", 1},
			{"ECDHE-RSA-AES128-GCM-SHA256", 2},
			{"ECDHE-ECDSA-AES256-GCM-SHA384", 3},
			{"ECDHE-RSA-AES256-GCM-SHA384", 4},
		},
	},
	"ELBSecurityPolicy-TLS13-1-2-2021-06": {
		name:      "ELBSecurityPolicy-TLS13-1-2-2021-06",
		protocols: []string{"TLSv1.3", "TLSv1.2"},
		ciphers: []sslPolicyCipher{
			{"TLS_AES_128_GCM_SHA256", 1},
			{"TLS_AES_256_GCM_SHA384", 2},
			{"TLS_CHACHA20_POLY1305_SHA256", 3},
			{"ECDHE-ECDSA-AES128-GCM-SHA256", 4},
			{"ECDHE-RSA-AES128-GCM-SHA256", 5},
		},
	},
}

// sslPolicyOrder is the deterministic order DescribeSSLPolicies returns the
// catalog in.
var sslPolicyOrder = []string{
	DefaultSslPolicy,
	"ELBSecurityPolicy-TLS13-1-2-2021-06",
}

// isKnownSslPolicy reports whether name is in the catalog.
func isKnownSslPolicy(name string) bool {
	_, ok := sslPolicyCatalog[name]
	return ok
}

// sslPolicyToSDK renders a catalog definition as an SDK SslPolicy.
func sslPolicyToSDK(def sslPolicyDef) *elbv2.SslPolicy {
	ciphers := make([]*elbv2.Cipher, 0, len(def.ciphers))
	for _, c := range def.ciphers {
		ciphers = append(ciphers, &elbv2.Cipher{
			Name:     aws.String(c.name),
			Priority: aws.Int64(c.priority),
		})
	}
	protocols := make([]*string, 0, len(def.protocols))
	for _, p := range def.protocols {
		protocols = append(protocols, aws.String(p))
	}
	return &elbv2.SslPolicy{
		Name:                       aws.String(def.name),
		SslProtocols:               protocols,
		Ciphers:                    ciphers,
		SupportedLoadBalancerTypes: []*string{aws.String("application")},
	}
}
