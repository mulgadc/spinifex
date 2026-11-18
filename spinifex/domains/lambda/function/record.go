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
	kvDeletedKeyPrefix     = "deleted."
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
	SubjectCreate          = "lambda.function.create"
	SubjectGet             = "lambda.function.get"
	SubjectCodeStored      = "lambda.function.code-stored"
	SubjectRollbackCreate  = "lambda.function.rollback-create"
	SubjectCodeUpdateBegin = "lambda.function.code-update-begin"
	SubjectCodeUpdated     = "lambda.function.code-updated"
	// SubjectConfigurationUpdated commits new settings for an Active function.
	SubjectConfigurationUpdated = "lambda.function.configuration-updated"
	// SubjectList pages the function records of one Region.
	SubjectList = "lambda.function.list"
	// SubjectDelete unbinds a function name and retains its cleanup obligation.
	SubjectDelete = "lambda.function.delete"
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

// CodeUpdateBeginRequest asks for an owed upload of new code for the function
// as it stood at Revision; RevisionID names the new code copy.
type CodeUpdateBeginRequest struct {
	Region       string `json:"region"`
	FunctionName string `json:"functionName"`
	FunctionID   string `json:"functionId"`
	Revision     uint64 `json:"revision"`
	RevisionID   string `json:"revisionId"`
}

// CodeUpdateBeginResponse is empty: success means the upload is owed.
type CodeUpdateBeginResponse struct{}

// CodeUpdatedRequest names the stored new copy for the function at Revision.
type CodeUpdatedRequest struct {
	Region       string `json:"region"`
	FunctionName string `json:"functionName"`
	FunctionID   string `json:"functionId"`
	Revision     uint64 `json:"revision"`
	RevisionID   string `json:"revisionId"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
}

// CodeUpdatedResponse returns the committed record and its revision.
type CodeUpdatedResponse struct {
	Record   Record `json:"record"`
	Revision uint64 `json:"revision"`
}

// Configuration is the complete set of settings an update leaves on the
// function; the gateway merges omitted members from the record it read.
type Configuration struct {
	Role        string `json:"role"`
	Handler     string `json:"handler"`
	Description string `json:"description,omitempty"`
	Timeout     int64  `json:"timeout"`
	MemorySize  int64  `json:"memorySize"`
	EnvCipher   string `json:"envCipher,omitempty"`
}

// ConfigurationUpdatedRequest replaces the settings of the function at Revision.
type ConfigurationUpdatedRequest struct {
	Region        string        `json:"region"`
	FunctionName  string        `json:"functionName"`
	FunctionID    string        `json:"functionId"`
	Revision      uint64        `json:"revision"`
	Configuration Configuration `json:"configuration"`
}

// ConfigurationUpdatedResponse returns the committed record and its revision.
type ConfigurationUpdatedResponse struct {
	Record   Record `json:"record"`
	Revision uint64 `json:"revision"`
}

// KVDeletedKey returns the key of the obligation a deleted function leaves
// behind. A recreated function receives a new random ID, and an obligation
// whose identity does not match the deleted function is refused.
func KVDeletedKey(region, functionID string) string {
	return kvDeletedKeyPrefix + region + "." + functionID
}

// Deletion is the retained cleanup obligation of a deleted function: its code
// copies and upload records are kept until a later owner proves them unused.
// It is written before the name is unbound, so it alone proves no deletion.
type Deletion struct {
	FormatVersion int       `json:"formatVersion"`
	AccountID     string    `json:"accountId"`
	Region        string    `json:"region"`
	FunctionName  string    `json:"functionName"`
	FunctionID    string    `json:"functionId"`
	RecordedAt    time.Time `json:"recordedAt"`
}

// ListRequest asks for one page of a Region's functions after Marker, which
// is empty for the first page or a NextMarker the owner returned.
type ListRequest struct {
	Region   string `json:"region"`
	Marker   string `json:"marker,omitempty"`
	MaxItems int    `json:"maxItems"`
}

// ListedFunction is one function record and its revision.
type ListedFunction struct {
	Record   Record `json:"record"`
	Revision uint64 `json:"revision"`
}

// ListResponse is one page; NextMarker is empty once the walk has ended.
type ListResponse struct {
	Functions  []ListedFunction `json:"functions"`
	NextMarker string           `json:"nextMarker,omitempty"`
}

// DeleteRequest unbinds the function the caller read at Revision.
type DeleteRequest struct {
	Region       string `json:"region"`
	FunctionName string `json:"functionName"`
	FunctionID   string `json:"functionId"`
	Revision     uint64 `json:"revision"`
}

// DeleteResponse is empty: success means the name was unbound.
type DeleteResponse struct{}
