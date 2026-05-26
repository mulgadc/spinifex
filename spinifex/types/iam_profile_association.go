package types

import "time"

// IamProfileDisassociateRequest is the fan-out payload published on
// ec2.IamProfileAssociation.disassociate. The daemon owning the target
// association mutates its vm.VM record and replies with Found=true; daemons
// that don't own it reply with Found=false so the gateway can early-exit the
// collector instead of waiting for the 3s timeout.
type IamProfileDisassociateRequest struct {
	AssociationId string `json:"association_id"`
}

// IamProfileReplaceRequest is the fan-out payload published on
// ec2.IamProfileAssociation.replace. The owning daemon validates AssociationId,
// generates a new association ID, and atomically swaps both fields on vm.VM.
type IamProfileReplaceRequest struct {
	AssociationId      string `json:"association_id"`
	InstanceProfileArn string `json:"instance_profile_arn"`
}

// IamProfileDescribeRequest is the fan-out payload published on
// ec2.IamProfileAssociation.describe. Filters are pre-parsed at the gateway
// to keep daemon handlers small. Empty filter slices match everything.
type IamProfileDescribeRequest struct {
	AssociationIds []string `json:"association_ids,omitempty"`
	InstanceIds    []string `json:"instance_ids,omitempty"`
	States         []string `json:"states,omitempty"`
}

// IamProfileAssociationResult is the daemon → gateway response for Associate
// (per-instance NATS path) and the per-association mutator response on
// Disassociate/Replace. Found=false signals "this daemon does not own the
// referenced AssociationId" — used as a NoOp on the fan-out subjects so the
// gateway's expectedNodes collector exits early.
type IamProfileAssociationResult struct {
	Found              bool      `json:"found"`
	AssociationId      string    `json:"association_id,omitempty"`
	InstanceId         string    `json:"instance_id,omitempty"`
	InstanceProfileArn string    `json:"instance_profile_arn,omitempty"`
	Timestamp          time.Time `json:"timestamp,omitzero"`
}

// IamProfileAssociationRecord is one entry in a Describe response. State is
// always "associated" in v1 (no async associating/disassociating transitions).
type IamProfileAssociationRecord struct {
	AssociationId      string    `json:"association_id"`
	InstanceId         string    `json:"instance_id"`
	InstanceProfileArn string    `json:"instance_profile_arn"`
	State              string    `json:"state"`
	Timestamp          time.Time `json:"timestamp,omitzero"`
}

// IamProfileDescribeResponse aggregates a daemon's matching associations. An
// empty Associations list is a valid response (no matches on that daemon) and
// counts toward the expectedNodes early-exit collector.
type IamProfileDescribeResponse struct {
	Associations []IamProfileAssociationRecord `json:"associations"`
}
