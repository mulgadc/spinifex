//go:build lambdatestlink

// Package testlink carries one controlled-test Lambda invocation from the
// gateway to the daemon on the same node and runs it in a fresh guest VM. It
// exists only in lambdatestlink builds; it holds no grant and is not the
// production executor.
package testlink

import "time"

// Subject is the request subject for one node's test-link daemon.
func Subject(node string) string { return "spinifex.lambda.testlink.invoke." + node }

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
)

// Request asks the daemon to run one invocation. CodeURL is a short-lived
// download link for the exact digest; it must never be logged or stored.
type Request struct {
	AttemptID   string `json:"attempt_id"`
	RequestID   string `json:"request_id"`
	FunctionARN string `json:"function_arn"`
	Region      string `json:"region"`
	AccountID   string `json:"account_id"`
	FunctionID  string `json:"function_id"`
	TimeoutMs   int64  `json:"timeout_ms"`
	MemoryMiB   int64  `json:"memory_mib"`
	CodeURL     string `json:"code_url"`
	CodeSize    int64  `json:"code_size"`
	CodeSHA256  string `json:"code_sha256"`
	Payload     []byte `json:"payload"`
}

// Reply is the daemon's answer. Error is a reason safe to log.
type Reply struct {
	Outcome       string `json:"outcome"`
	Payload       []byte `json:"payload,omitempty"`
	FunctionError string `json:"function_error,omitempty"`
	Error         string `json:"error,omitempty"`
}
