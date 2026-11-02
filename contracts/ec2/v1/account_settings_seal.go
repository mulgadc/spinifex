package ec2v1

// SealAccountSettingsSubject is the request/reply subject on which a daemon
// seals one account's EC2 settings record. Sealing is irreversible and refused
// unless IAM already holds the account as TERMINATING.
const SealAccountSettingsSubject = "ec2.v1.SealAccountSettings"

// AccountSettingsDispositionSealed is the only disposition a successful reply
// carries: a reply is sent only on success, so there is no other value.
const AccountSettingsDispositionSealed = "sealed"

// SealAccountSettingsRequest asks for one account's EC2 settings to be
// sealed. CorrelationRef is an opaque caller reference echoed for logs; it
// grants nothing and is never a fence or authority to write.
type SealAccountSettingsRequest struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
}

// SealAccountSettingsReply confirms accountID's settings record was sealed at
// Revision, the stream leader's accepted write for that seal. A failure is
// the standard awserrors error payload, not this type.
type SealAccountSettingsReply struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
	Disposition    string `json:"disposition"`
	Revision       uint64 `json:"revision"`
}
