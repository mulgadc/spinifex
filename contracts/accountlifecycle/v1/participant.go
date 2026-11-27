// Package accountlifecyclev1 is the contract between the account-termination
// engine and each participant domain: the declaration a participant publishes,
// and the inventory and termination command it serves. See README.md.
package accountlifecyclev1

import (
	"strings"
	"time"
)

// ContractVersion is the version of the inventory and command shapes below.
const ContractVersion = 1

// Class statuses. A class is clean only when the owner read every record of
// that class completely and found none for the account.
const (
	StatusClean      = "clean"
	StatusPresent    = "present"
	StatusIncomplete = "incomplete"
)

// Reasons are fixed categories. They never carry resource names or wire error
// text.
const (
	ReasonStoreError       = "store-error"
	ReasonUnreadableRecord = "unreadable-record"
	ReasonDraining         = "draining"
	ReasonOwnerNotStopped  = "owner-not-stopped"
	ReasonNotTerminating   = "account-not-terminating"
	ReasonSealFailed       = "seal-failed"
)

// InventorySubject is the read-only inventory route of participant name.
func InventorySubject(name string) string {
	return "accountlifecycle.v1.participant." + name + ".inventory"
}

// TerminateSubject is the termination command route of participant name.
func TerminateSubject(name string) string {
	return "accountlifecycle.v1.participant." + name + ".terminate"
}

// ValidParticipantName reports whether name is one lower-case NATS subject
// token, so a subject built from it addresses exactly one participant.
func ValidParticipantName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return !strings.HasPrefix(name, "-")
}

// Declaration is what a participant publishes about itself in its own
// contract package. DeclarationVersion rises whenever Classes change.
type Declaration struct {
	Name                     string
	DeclarationVersion       int
	Classes                  []string
	InventoryContractVersion int
	CommandContractVersion   int
	DrainBudget              time.Duration
}

// InventoryRequest asks for one account's remaining owned inventory.
// CorrelationRef is opaque and grants nothing.
type InventoryRequest struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
}

// ClassReport is one declared class's remaining count and status.
type ClassReport struct {
	Class     string   `json:"class"`
	Remaining int      `json:"remaining"`
	Status    string   `json:"status"`
	Reasons   []string `json:"reasons,omitempty"`
}

// InventoryReply reports every declared class, read from the owner's
// authoritative stores.
type InventoryReply struct {
	AccountID          string        `json:"account_id"`
	Participant        string        `json:"participant"`
	DeclarationVersion int           `json:"declaration_version"`
	ContractVersion    int           `json:"contract_version"`
	Classes            []ClassReport `json:"classes"`
}

// TerminateRequest runs one pass of the participant's termination command.
// OperationID and Fence are empty in the interim engine.
type TerminateRequest struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
	OperationID    string `json:"operation_id,omitempty"`
	Fence          uint64 `json:"fence,omitempty"`
}

// BlockedItem is one class that the command could not finish, and why.
type BlockedItem struct {
	Class  string `json:"class"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// TerminateReply carries per-class dispositions in the inventory vocabulary.
type TerminateReply struct {
	AccountID          string        `json:"account_id"`
	Participant        string        `json:"participant"`
	DeclarationVersion int           `json:"declaration_version"`
	ContractVersion    int           `json:"contract_version"`
	Classes            []ClassReport `json:"classes"`
	Blocked            []BlockedItem `json:"blocked,omitempty"`
}
