// Package function holds the Lambda function record and the daemon-owned
// metadata service that creates and updates it.
package function

import "time"

// FormatVersion is the only record layout this build reads or writes.
const FormatVersion = 1

// Function states and the code copy states.
const (
	StatePending = "Pending"
	StateActive  = "Active"
	StateFailed  = "Failed"

	CodeStoring = "storing"
	CodeStored  = "stored"
	CodeFailed  = "failed"
)

// Reason reported while a function waits for a runtime image to exist.
const (
	ReasonCodeCreating = "Creating"
	ReasonCreating     = "The function is waiting for its runtime image."
)

// Code describes the stored copy of a function's code.
type Code struct {
	RevisionID string `json:"revisionId"`
	SHA256     string `json:"sha256,omitempty"`
	Size       int64  `json:"size,omitempty"`
	State      string `json:"state"`
}

// Record is one function. Its KV key both binds the name and holds it, so name
// reservation and the record are a single atomic write.
type Record struct {
	FormatVersion   int    `json:"formatVersion"`
	FunctionID      string `json:"functionId"`
	AccountID       string `json:"accountId"`
	Region          string `json:"region"`
	FunctionName    string `json:"functionName"`
	Role            string `json:"role"`
	Runtime         string `json:"runtime"`
	Handler         string `json:"handler"`
	Description     string `json:"description,omitempty"`
	Timeout         int64  `json:"timeout"`
	MemorySize      int64  `json:"memorySize"`
	EnvCipher       string `json:"envCipher,omitempty"`
	State           string `json:"state"`
	StateReason     string `json:"stateReason,omitempty"`
	StateReasonCode string `json:"stateReasonCode,omitempty"`
	Code            Code   `json:"code"`
	// ClaimExpiresAt bounds how long a record still storing its code holds the name.
	ClaimExpiresAt time.Time `json:"claimExpiresAt"`
	CreatedAt      time.Time `json:"createdAt"`
	LastModified   time.Time `json:"lastModified"`
}

// KV bucket and key layout. The bucket is per account and owned by the daemon.
const (
	KVBucketAccountPrefix  = "lambda-account-"
	KVBucketAccountHistory = 1
	kvFunctionKeyPrefix    = "fn."
	kvUploadKeyPrefix      = "upload."
)

// KVUploadKey returns the key of the record that tracks one code upload. The
// bucket is shared across Regions, so the Region is part of the key.
func KVUploadKey(region, functionID, revisionID string) string {
	return kvUploadKeyPrefix + region + "." + functionID + "." + revisionID
}

// Upload states. An owed upload may have a code copy that no function record
// names; it stays owed until the owner records otherwise.
const (
	UploadOwed     = "owed"
	UploadAttached = "attached"
)

// Upload tracks one code upload. A create begun through this owner writes it
// before the copy; records and copies from older owners have none.
type Upload struct {
	FormatVersion int       `json:"formatVersion"`
	AccountID     string    `json:"accountId"`
	Region        string    `json:"region"`
	FunctionName  string    `json:"functionName"`
	FunctionID    string    `json:"functionId"`
	RevisionID    string    `json:"revisionId"`
	State         string    `json:"state"`
	CreatedAt     time.Time `json:"createdAt"`
	// AttachedRevision is the function record revision that names the copy.
	AttachedRevision uint64 `json:"attachedRevision,omitempty"`
}

// KVAccountBucket returns the per-account JetStream KV bucket name.
func KVAccountBucket(accountID string) string { return KVBucketAccountPrefix + accountID }

// KVFunctionKey returns the key that binds a function name in a Region and
// holds its record. JetStream is shared across Regions, so the Region is part
// of the key.
func KVFunctionKey(region, name string) string { return kvFunctionKeyPrefix + region + "." + name }

// NATS subjects served by the daemon's metadata service.
const (
	SubjectCreate         = "lambda.function.create"
	SubjectGet            = "lambda.function.get"
	SubjectCodeStored     = "lambda.function.code-stored"
	SubjectRollbackCreate = "lambda.function.rollback-create"
)

// CreateRequest carries the record the gateway built; the daemon owns the
// state, claim and timestamps and overwrites whatever the request holds there.
type CreateRequest struct {
	Region string `json:"region"`
	Record Record `json:"record"`
}

// CreateResponse returns the KV revision the later steps must present.
type CreateResponse struct {
	Revision uint64 `json:"revision"`
}

// GetRequest names the function.
type GetRequest struct {
	Region       string `json:"region"`
	FunctionName string `json:"functionName"`
}

// GetResponse holds the record when Found.
type GetResponse struct {
	Found    bool   `json:"found"`
	Record   Record `json:"record"`
	Revision uint64 `json:"revision"`
}

// CodeStoredRequest records that the code copy landed.
type CodeStoredRequest struct {
	Region       string `json:"region"`
	FunctionName string `json:"functionName"`
	Revision     uint64 `json:"revision"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
}

// CodeStoredResponse returns the record as stored and its revision.
type CodeStoredResponse struct {
	Record   Record `json:"record"`
	Revision uint64 `json:"revision"`
}

// RollbackCreateRequest withdraws a create whose own code write failed.
type RollbackCreateRequest struct {
	Region       string `json:"region"`
	FunctionName string `json:"functionName"`
	Revision     uint64 `json:"revision"`
}

// RollbackCreateResponse is empty: success means the name is free.
type RollbackCreateResponse struct{}
