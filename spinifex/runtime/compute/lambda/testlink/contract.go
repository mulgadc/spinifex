//go:build lambdatestlink

// Package testlink runs one controlled-test Lambda invocation in a fresh guest
// VM on the gateway's node. It exists only in lambdatestlink builds and is not
// the production executor. The guest checks the grant it is given but cannot
// verify that the grant's owner holds authority.
package testlink

import (
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
)

// Outcomes a daemon reply can carry.
const (
	// OutcomeDone means the guest returned a result and the VM stop is proven.
	OutcomeDone = "done"
	// OutcomeRefused means the daemon launched nothing for this request.
	OutcomeRefused = "refused"
	// OutcomeUnknown means a VM may have run; nothing about the result holds.
	OutcomeUnknown = "unknown"
)

// Dev-only bounds for the test path; they are not AWS limits.
const (
	MaxCodeBytes = 16 << 20
	// BootAllowance is added to the function timeout to bound the whole VM run.
	BootAllowance = 15 * time.Second
	CodeURLTTL    = 2 * time.Minute
	// LaunchWindow is how long a grant may wait for the daemon to start it.
	LaunchWindow = 30 * time.Second
)

// Request asks the daemon to run one invocation. CodeURL is a short-lived
// download link for the exact digest; it must never be logged or stored.
type Request struct {
	Grant       execution.Grant `json:"grant"`
	AttemptID   string          `json:"attempt_id"`
	RequestID   string          `json:"request_id"`
	FunctionARN string          `json:"function_arn"`
	Handler     string          `json:"handler"`
	Region      string          `json:"region"`
	AccountID   string          `json:"account_id"`
	FunctionID  string          `json:"function_id"`
	RevisionID  string          `json:"revision_id"`
	TimeoutMs   int64           `json:"timeout_ms"`
	MemoryMiB   int64           `json:"memory_mib"`
	CodeURL     string          `json:"code_url"`
	CodeSize    int64           `json:"code_size"`
	CodeSHA256  string          `json:"code_sha256"`
	// ImageDigest is the runtime image the gateway expects to run the code.
	ImageDigest string `json:"image_digest"`
	Payload     []byte          `json:"payload"`

	// cred arrives beside the request on the control socket, never inside it.
	cred Credential
}

// Reply is the daemon's answer. Error is a reason safe to log.
type Reply struct {
	Outcome       string `json:"outcome"`
	Payload       []byte `json:"payload,omitempty"`
	FunctionError string `json:"function_error,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Identity is the request and code identity the grant must authorise.
func (r Request) Identity() execution.Identity {
	return execution.Identity{RequestID: r.RequestID, AccountID: r.AccountID, Region: r.Region,
		FunctionID: r.FunctionID, RevisionID: r.RevisionID, CodeSHA256: r.CodeSHA256}
}
