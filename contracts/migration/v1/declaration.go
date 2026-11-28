// Package migrationv1 is the migration domain's cross-process contract: its
// account-termination participant declaration. See README.md.
package migrationv1

import (
	"time"

	accountlifecyclev1 "github.com/mulgadc/spinifex/contracts/accountlifecycle/v1"
)

// ParticipantName is the stable participant name. It is never reused.
const ParticipantName = "migration"

// Owned record classes. Each is the kind prefix of a record key.
const (
	ClassConnection = "connection"
	ClassAssessment = "assessment"
	ClassCopyTask   = "copy-task"
)

// DeclarationVersion rises whenever the owned classes change.
const DeclarationVersion = 1

// DrainBudget bounds how long the engine repeats the command before it
// reports the participant blocked. A blocked run keeps the account.
const DrainBudget = 2 * time.Minute

// Declaration is the participant declaration the engine composes.
func Declaration() accountlifecyclev1.Declaration {
	return accountlifecyclev1.Declaration{
		Name:                     ParticipantName,
		DeclarationVersion:       DeclarationVersion,
		Classes:                  []string{ClassConnection, ClassAssessment, ClassCopyTask},
		InventoryContractVersion: accountlifecyclev1.ContractVersion,
		CommandContractVersion:   accountlifecyclev1.ContractVersion,
		DrainBudget:              DrainBudget,
	}
}
