package admissionv1

import "strings"

// AccountQuotaOverrideSealCapabilityVersion is the version this binary reports.
// Version 1: every quota override writer in the binary is revision-guarded and
// refuses a sealed record, and the seal subject is served.
const AccountQuotaOverrideSealCapabilityVersion = 1

// AccountQuotaOverrideSealCapabilitySubject is one gateway's read-only
// seal-capability route, addressed per node so a probe reaches exactly the
// node it asks about.
func AccountQuotaOverrideSealCapabilitySubject(node string) string {
	return "admission.v1.AccountQuotaOverrideSealCapability." + node
}

// ValidSealCapabilityNode reports whether node is exactly one NATS subject
// token. Responder and probe both check it before building a subject, so
// neither can address a wildcard or another node's route.
func ValidSealCapabilityNode(node string) bool {
	return node != "" && !strings.ContainsAny(node, ".*>\t\n\r ")
}

// AccountQuotaOverrideSealCapabilityRequest asks node whether it supports the
// quota override seal. CorrelationRef is an opaque caller reference echoed for
// logs; it grants nothing and is never a fence or authority to write.
type AccountQuotaOverrideSealCapabilityRequest struct {
	Node           string `json:"node"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
}

// AccountQuotaOverrideSealCapabilityReply reports the responding node's
// capability version. It carries no account data and proves nothing beyond
// the moment it was sent.
type AccountQuotaOverrideSealCapabilityReply struct {
	Node              string `json:"node"`
	CapabilityVersion int    `json:"capability_version"`
	CorrelationRef    string `json:"correlation_ref,omitempty"`
}
