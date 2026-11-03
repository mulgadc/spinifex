package accountlifecycle

import (
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
)

// QuiesceEvidence is what the engine read after QuiesceAccount returned.
type QuiesceEvidence struct {
	OperationID string
	Fence       uint64
	Generation  uint64

	// FinalRereadGuaranteed is set by a run that re-reads the owner inventory
	// after its cleanup and before anything is finalised: an exercise, and an
	// ordinary delete before it removes the account record. A caller that does
	// not guarantee it records no scope.
	FinalRereadGuaranteed bool

	// Account is the IAM record read back after the quiesce.
	Account     *Account
	ReadbackErr error
	Inventory   *ec2v1.PlatformInventoryReply
	EvaluatedAt time.Time
}

// enforcementConsumers are the consumers with an accepted contract for the
// account admission fact. Predastore is deliberately absent: it has none.
var enforcementConsumers = []operatorv1.ConsumerBasis{
	{Consumer: "awsgw-admission", Basis: "ADR-0005:S7"},
	{Consumer: "ecr-registry", Basis: "ADR-0005:S7"},
	{Consumer: "sts-mint-gate", Basis: "ADR-0008:S9 (IAM-owned)"},
}

// accountedWriters cannot be fenced and are covered by the owner inventory.
var accountedWriters = []operatorv1.AccountedWriter{
	{Writer: "daemon-nats-handlers", Basis: "owner-inventory", Ref: "mulga-tpoz4"},
}

// ec2PlatformKinds are the kinds the first scope covers. The inventory's own
// tag-index kind belongs to a different register row and is not part of it.
var ec2PlatformKinds = []string{
	ec2v1.PlatformKindKeyPair,
	ec2v1.PlatformKindPlacementGroup,
	ec2v1.PlatformKindLaunchTemplate,
}

type unresolvedEntry struct {
	scope, class, reason string
}

// otherScopes lists every scope the first exercise does not cover, by name.
// A register row family missing from here would read as quiesced by omission.
var otherScopes = []unresolvedEntry{
	{"ec2-instances", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"ec2-spot-requests", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"ebs-volumes", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"ec2-snapshots-images", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"ec2-vpc", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"host-network", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"ec2-account-settings", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"ec2-tag-index", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"iam", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"elbv2", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"acm", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"eks", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"ecs", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"rds", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"dns", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"ochre", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"quota", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"idempotency", operatorv1.UnresolvedClassWriter, operatorv1.ReasonNoCompleteOwnerInventory},
	{"s3", operatorv1.UnresolvedClassConsumer, operatorv1.ReasonNoAcceptedEnforcement},
	{"ecr", operatorv1.UnresolvedClassConsumer, operatorv1.ReasonNoAcceptedEnforcement},
	{"lambda", operatorv1.UnresolvedClassWriter, operatorv1.ReasonParticipantNotDeclared},
	{"sqs", operatorv1.UnresolvedClassWriter, operatorv1.ReasonParticipantNotDeclared},
}

// EvaluateQuiescence applies ADR-0008:S9's three conditions to the evidence and
// records what holds. A fence the read-back does not confirm stops everything
// else; otherwise every scope is listed. A kind is quiesced only when the final
// re-read is guaranteed and its owner inventory was complete.
func EvaluateQuiescence(ev QuiesceEvidence) operatorv1.IAMQuiescence {
	out := operatorv1.IAMQuiescence{
		OperationID:   ev.OperationID,
		Fence:         ev.Fence,
		Generation:    ev.Generation,
		QuiescedScope: []string{},
		Unresolved:    []operatorv1.UnresolvedScope{},
		Consumers:     []operatorv1.ConsumerBasis{},
		Writers:       []operatorv1.AccountedWriter{},
		Inventory:     []operatorv1.InventoryStatus{},
		EvaluatedAt:   ev.EvaluatedAt.UTC().Format(time.RFC3339),
	}

	if reason, ok := fenceReadback(ev); !ok {
		out.Unresolved = append(out.Unresolved, operatorv1.UnresolvedScope{
			Scope: "all", Class: operatorv1.UnresolvedClassFence, Reason: reason,
		})
		return out
	}

	out.Consumers = append(out.Consumers, enforcementConsumers...)
	out.Writers = append(out.Writers, accountedWriters...)

	for _, kind := range ec2PlatformKinds {
		status, complete := kindInventory(ev.Inventory, kind)
		out.Inventory = append(out.Inventory, operatorv1.InventoryStatus{Kind: kind, Status: status})
		switch {
		case !complete:
			out.Unresolved = append(out.Unresolved, writerEntry("ec2-"+kind, operatorv1.ReasonOwnerInventoryIncomplete))
		case !ev.FinalRereadGuaranteed:
			out.Unresolved = append(out.Unresolved, writerEntry("ec2-"+kind, operatorv1.ReasonFinalInventoryRereadNotWired))
		default:
			out.QuiescedScope = append(out.QuiescedScope, kind)
		}
	}
	for _, entry := range otherScopes {
		out.Unresolved = append(out.Unresolved, operatorv1.UnresolvedScope{
			Scope: entry.scope, Class: entry.class, Reason: entry.reason,
		})
	}
	return out
}

func writerEntry(scope, reason string) operatorv1.UnresolvedScope {
	return operatorv1.UnresolvedScope{Scope: scope, Class: operatorv1.UnresolvedClassWriter, Reason: reason}
}

// fenceReadback is condition 1: the operation record and the IAM account record
// hold the same operation, fence and generation.
func fenceReadback(ev QuiesceEvidence) (reason string, ok bool) {
	if ev.ReadbackErr != nil || ev.Account == nil {
		return operatorv1.ReasonIAMReadbackFailed, false
	}
	op := ev.Account.TerminationOp
	if ev.Account.Status != AccountStatusTerminating || op == nil ||
		op.OperationID != ev.OperationID || op.Fence != ev.Fence || ev.Account.Generation != ev.Generation {
		return operatorv1.ReasonFenceMismatch, false
	}
	return "", true
}

// kindInventory reports one kind's status and whether the owner scan was
// complete. A missing or repeated kind, or no reply at all, is incomplete.
func kindInventory(reply *ec2v1.PlatformInventoryReply, kind string) (status string, complete bool) {
	if reply == nil {
		return ec2v1.PlatformStatusScanIncomplete, false
	}
	var found *ec2v1.PlatformKindReport
	for i := range reply.Kinds {
		if reply.Kinds[i].Kind != kind {
			continue
		}
		if found != nil {
			return ec2v1.PlatformStatusScanIncomplete, false
		}
		found = &reply.Kinds[i]
	}
	if found == nil || found.Status == ec2v1.PlatformStatusScanIncomplete {
		return ec2v1.PlatformStatusScanIncomplete, false
	}
	return found.Status, true
}
