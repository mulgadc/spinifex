// Package addon owns the EKS managed add-on: its persisted record and staged
// manifest, the bundled catalogue, the status transitions and manifest staging.
package addon

import (
	"errors"
	"time"
)

// Status mirrors the AWS EKS addon.status enum verbatim.
type Status string

const (
	StatusCreating     Status = "CREATING"
	StatusActive       Status = "ACTIVE"
	StatusUpdating     Status = "UPDATING"
	StatusDeleting     Status = "DELETING"
	StatusDegraded     Status = "DEGRADED"
	StatusCreateFailed Status = "CREATE_FAILED"
)

// Record is the persisted state for a managed add-on: lifecycle status, IRSA
// role, and opaque configuration the installer needs.
type Record struct {
	AddonName             string            `json:"addonName"`
	AddonVersion          string            `json:"addonVersion"`
	Status                Status            `json:"status"`
	ServiceAccountRoleArn string            `json:"serviceAccountRoleArn,omitempty"`
	ConfigurationValues   string            `json:"configurationValues,omitempty"`
	Health                string            `json:"health,omitempty"`
	Arn                   string            `json:"arn"`
	Tags                  map[string]string `json:"tags,omitempty"`
	CreatedAt             time.Time         `json:"createdAt"`
	ModifiedAt            time.Time         `json:"modifiedAt"`
}

// Manifest is the staged delivery descriptor stored beside the record: the
// bundled add-on and version plus the operator-supplied config the guest
// renders the baked manifests with.
type Manifest struct {
	AddonName             string `json:"addonName"`
	AddonVersion          string `json:"addonVersion"`
	ServiceAccountRoleArn string `json:"serviceAccountRoleArn,omitempty"`
	ConfigurationValues   string `json:"configurationValues,omitempty"`
}

// ErrNotFound is returned when no record exists for the add-on. Callers
// translate it to ResourceNotFoundException at the service boundary.
var ErrNotFound = errors.New("eks: addon not found")

// ErrExists is returned by Create when a record already exists for the name.
var ErrExists = errors.New("eks: addon already exists")
