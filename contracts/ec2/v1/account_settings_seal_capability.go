package ec2v1

import "strings"

// AccountSettingsSealCapabilityVersion is the version this binary reports.
// Version 1: every EC2 account-settings writer in the binary is
// revision-guarded and refuses a sealed record, and the seal subject is served.
const AccountSettingsSealCapabilityVersion = 1

// AccountSettingsSealCapabilitySubject is one daemon's read-only
// seal-capability route, addressed per node so a probe reaches exactly the
// node it asks about.
func AccountSettingsSealCapabilitySubject(node string) string {
	return "ec2.v1.AccountSettingsSealCapability." + node
}

// ValidSealCapabilityNode reports whether node is exactly one NATS subject
// token. Responder and probe both check it before building a subject, so
// neither can address a wildcard or another node's route.
func ValidSealCapabilityNode(node string) bool {
	return node != "" && !strings.ContainsAny(node, ".*>\t\n\r ")
}

// AccountSettingsSealCapabilityRequest asks node whether it supports the
// account-settings seal. CorrelationRef is an opaque caller reference echoed
// for logs; it grants nothing and is never a fence or authority to write.
type AccountSettingsSealCapabilityRequest struct {
	Node           string `json:"node"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
}

// AccountSettingsSealCapabilityReply reports the responding node's capability
// version. It carries no account data and proves nothing beyond the moment it
// was sent.
type AccountSettingsSealCapabilityReply struct {
	Node              string `json:"node"`
	CapabilityVersion int    `json:"capability_version"`
	CorrelationRef    string `json:"correlation_ref,omitempty"`
}
