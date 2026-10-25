// Package admissionv1 holds the admission domain's cross-process contract:
// the subjects, request/reply shapes and wire vocabulary the quota owner and
// its consumers share.
package admissionv1

// SealAccountQuotaOverrideSubject is the request/reply subject on which a
// gateway seals one account's quota override record. Sealing is irreversible
// and refused unless IAM already holds the account as TERMINATING.
const SealAccountQuotaOverrideSubject = "admission.v1.SealAccountQuotaOverride"

// AccountQuotaOverrideDispositionSealed is the only disposition a successful
// reply carries: a reply is sent only on success, so there is no other value.
const AccountQuotaOverrideDispositionSealed = "sealed"

// SealAccountQuotaOverrideRequest asks for one account's quota override to be
// sealed. CorrelationRef is an opaque caller reference echoed for logs; it
// grants nothing and is never a fence or authority to write.
type SealAccountQuotaOverrideRequest struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
}

// SealAccountQuotaOverrideReply confirms accountID's quota override record
// was sealed at Revision, the stream leader's accepted write for that seal.
// A failure is the standard awserrors error payload, not this type.
type SealAccountQuotaOverrideReply struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
	Disposition    string `json:"disposition"`
	Revision       uint64 `json:"revision"`
}
