// Package accountteardown removes everything an account owns, in dependency
// order, and then the account itself.
//
// It is deliberately separate from the handlers it drives. Teardown is the one
// caller that must be able to delete a resource the customer-facing API would
// refuse — an attached volume, an instance whose node stopped answering — and
// keeping that ability in its own package means no ordinary request path can
// reach it by accident.
package accountteardown

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Stage groups resources that must all be gone before the next group is
// touched. The ordering is the whole reason teardown converges: it is what
// stops a volume from being undeletable because an instance still holds it.
type Stage int

const (
	// StageCompute releases everything that holds an attachment: instances,
	// spot requests, container and database workloads.
	StageCompute Stage = iota + 1

	// StageAttachments verifies stage 1 actually landed. Detaches are
	// asynchronous, so "the instance is gone" and "the volume is free" are
	// separate facts and storage deletion depends on the second one.
	StageAttachments

	// StageStorage removes snapshots, then volumes, then images and buckets.
	StageStorage

	// StageNetwork unwinds the VPC in reverse creation order.
	StageNetwork

	// StagePlatform removes account-scoped odds and ends with no ordering
	// relationship to each other.
	StagePlatform

	// StageIdentity removes credentials and IAM records. Access keys go first:
	// it is the second quiesce, and it holds even if the status gate does not.
	StageIdentity

	// StageAccount removes the account's own counters and reservations. The
	// account record itself is deleted by the engine after this stage.
	StageAccount
)

var stageNames = map[Stage]string{
	StageCompute:     "compute",
	StageAttachments: "attachments",
	StageStorage:     "storage",
	StageNetwork:     "network",
	StagePlatform:    "platform",
	StageIdentity:    "identity",
	StageAccount:     "account",
}

func (s Stage) String() string {
	if name, ok := stageNames[s]; ok {
		return name
	}
	return fmt.Sprintf("stage(%d)", int(s))
}

// Stages returns every stage in teardown order.
func Stages() []Stage {
	return []Stage{
		StageCompute, StageAttachments, StageStorage,
		StageNetwork, StagePlatform, StageIdentity, StageAccount,
	}
}

// Resource is one thing to delete. Detail carries whatever an operator needs
// to understand why it is here or why it will not go — an attachment, a
// dependency, the node holding it — and is never load-bearing.
type Resource struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Detail string `json:"detail,omitempty"`
}

func (r Resource) String() string {
	if r.Detail == "" {
		return r.Kind + " " + r.ID
	}
	return r.Kind + " " + r.ID + " (" + r.Detail + ")"
}

// Reaper enumerates and deletes one kind of resource for one account.
//
// List must be safe to call repeatedly: the engine re-lists after deleting to
// decide whether a stage has actually drained, because most deletes here are
// asynchronous.
//
// Delete must be idempotent. A resource that is already gone is a success, not
// a NotFound — teardown re-runs after a crash and would otherwise never finish.
type Reaper interface {
	Kind() string
	Stage() Stage
	List(ctx context.Context, accountID string) ([]Resource, error)
	Delete(ctx context.Context, accountID string, resource Resource, force bool) error
}

// AccountStore is the slice of the IAM service teardown needs. Narrow on
// purpose: this package must not be able to create anything.
type AccountStore interface {
	GetAccount(accountID string) (*Account, error)
	SetAccountStatus(accountID, status string) (*Account, error)
	DeleteAccount(accountID string) error
}

// Account mirrors the IAM account record. Redeclared rather than imported so
// the engine's tests do not need an IAM service to run.
type Account struct {
	AccountID   string
	AccountName string
	Status      string
}

// Request is one teardown.
type Request struct {
	AccountID string

	// AccountName must match the stored record. A mistyped account id then
	// fails closed instead of emptying a real tenant.
	AccountName string

	// Force escalates past state guards, hard-destroys on timeout, and treats
	// an already-missing resource as deleted. It never reorders the stages and
	// never widens scope beyond AccountID.
	Force bool

	// DryRun reports what would be deleted and touches nothing.
	DryRun bool
}

// StageResult is what happened in one stage.
type StageResult struct {
	Stage   Stage      `json:"stage"`
	Deleted []Resource `json:"deleted,omitempty"`
	Stuck   []Stuck    `json:"stuck,omitempty"`
	Elapsed string     `json:"elapsed"`
}

// Stuck is a resource teardown could not remove, and why. It is the most
// valuable thing this package produces: each one is a bug in a delete path.
type Stuck struct {
	Resource Resource `json:"resource"`
	Reason   string   `json:"reason"`
}

// Result is the outcome of a teardown or a dry run.
type Result struct {
	AccountID   string        `json:"accountId"`
	AccountName string        `json:"accountName"`
	DryRun      bool          `json:"dryRun"`
	Forced      bool          `json:"forced"`
	Stages      []StageResult `json:"stages"`

	// AccountDeleted is false whenever anything was left stuck: the account
	// stays TERMINATING so the residue stays attributable to it.
	AccountDeleted bool `json:"accountDeleted"`
}

// StuckCount totals resources left behind across every stage.
func (r *Result) StuckCount() int {
	total := 0
	for _, stage := range r.Stages {
		total += len(stage.Stuck)
	}
	return total
}

// DeletedCount totals resources removed across every stage.
func (r *Result) DeletedCount() int {
	total := 0
	for _, stage := range r.Stages {
		total += len(stage.Deleted)
	}
	return total
}

// ErrAccountNameMismatch reports a confirmation that did not match the stored
// account name.
var ErrAccountNameMismatch = errors.New("account name does not match the account id")

// ErrProtectedAccount reports an attempt to delete the system or super-admin
// account. No credential grants this.
var ErrProtectedAccount = errors.New("account is protected and cannot be deleted")

// ErrResourcesStuck reports a teardown that drained everything it could and
// still found resources it could not remove.
var ErrResourcesStuck = errors.New("teardown left resources behind")

// protectedAccountIDs are the two accounts no teardown may ever remove. They
// mirror admin.SystemAccountID and admin.DefaultAccountID; a test asserts the
// two lists agree so a change to either is caught rather than diverging.
var protectedAccountIDs = map[string]string{
	"000000000000": "system",
	"000000000001": "super admin",
}

// Timeouts bounds how long teardown will wait for a stage to drain.
type Timeouts struct {
	// StageDrain is the total budget for one stage, including re-listing after
	// deletes. Exceeding it marks whatever is left as stuck rather than
	// blocking the whole teardown on one resource.
	StageDrain time.Duration

	// DrainPoll is the gap between re-listing a stage that has not emptied.
	DrainPoll time.Duration
}

// DefaultTimeouts are sized for instance termination, which is the long pole:
// a guest gets a graceful shutdown before the hypervisor takes the VM away.
func DefaultTimeouts() Timeouts {
	return Timeouts{StageDrain: 5 * time.Minute, DrainPoll: 5 * time.Second}
}
