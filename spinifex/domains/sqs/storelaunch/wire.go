package storelaunch

import (
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
)

// EnsureSubjectPrefix addresses one node's store launcher. The subject has no
// queue group, so only the named node answers and the store stays pinned.
const EnsureSubjectPrefix = "sqs.store.Ensure.v1."

// EnsureRequest asks a node to ensure its Region's SQS store guest.
type EnsureRequest struct {
	NodeID        string `json:"NodeId"`
	InstanceType  string `json:"InstanceType"`
	VolumeSizeGiB int64  `json:"VolumeSizeGiB"`
}

// EnsureResponse names the bound store VM and its address. Bound is not
// ready: the worker's own authenticated check decides that.
type EnsureResponse struct {
	NodeID     string `json:"NodeId"`
	Region     string `json:"Region"`
	InstanceID string `json:"InstanceId"`
	Address    string `json:"Address"`
	// The instance type the store was launched with, fixed by its first request.
	InstanceType string `json:"InstanceType"`
	State        string `json:"State"`
}

// StateBoundResponse is the only state an EnsureResponse reports.
const StateBoundResponse = "bound"

// EnsureSubject is the subject node's store launcher listens on.
func EnsureSubject(node string) (string, error) {
	if !workerlaunch.ValidNodeID(node) {
		return "", fmt.Errorf("%w: invalid node ID", ErrIncomplete)
	}
	return EnsureSubjectPrefix + node, nil
}
