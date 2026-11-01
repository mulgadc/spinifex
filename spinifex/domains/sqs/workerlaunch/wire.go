package workerlaunch

import (
	"fmt"
	"regexp"
)

// EnsureSubjectPrefix addresses one node's worker launcher. The subject has no
// queue group, so only the named node answers and the worker stays pinned.
const EnsureSubjectPrefix = "sqs.worker.Ensure.v1."

// EnsureRequest asks a node to ensure its Region's shared SQS worker.
type EnsureRequest struct {
	NodeID       string `json:"NodeId"`
	InstanceType  string `json:"InstanceType"`
	VolumeSizeGiB int64  `json:"VolumeSizeGiB"`
}

// EnsureResponse names the bound worker VM. Bound is not ready: it says
// nothing about the guest's boot, its key or the store.
type EnsureResponse struct {
	NodeID     string `json:"NodeId"`
	Region     string `json:"Region"`
	InstanceID string `json:"InstanceId"`
	Generation uint64 `json:"Generation"`
	// The instance type the worker was launched with, fixed for its generation.
	InstanceType string `json:"InstanceType"`
	State        string `json:"State"`
}

// StateBound is the only state an EnsureResponse reports.
const StateBound = "bound"

var nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidNodeID reports whether id is safe to use as a single subject token.
func ValidNodeID(id string) bool { return nodeIDPattern.MatchString(id) }

// EnsureSubject is the subject node's launcher listens on.
func EnsureSubject(node string) (string, error) {
	if !ValidNodeID(node) {
		return "", fmt.Errorf("%w: invalid node ID", ErrIncomplete)
	}
	return EnsureSubjectPrefix + node, nil
}
