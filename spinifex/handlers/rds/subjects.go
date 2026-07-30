package handlers_rds

import "fmt"

// Layer-2 bus subjects. Instance identity is {accountID}.{dbInstanceIdentifier},
// matching the KV layout and deliberately not the internal EC2 instance ID,
// which changes on every VM replace while the subject must not.
const (
	busPrefix = "rds.bus."

	// What the daemon subscribes on; the account and DB segments are
	// addressing, and the payload is authoritative.
	SubjectRegisterWildcard = busPrefix + "*.*.register"
	SubjectHealthWildcard   = busPrefix + "*.*.health"

	// A Layer-1 subject, not a bus one: serving bootstrap config is a
	// control-plane read rather than reconciler↔agent traffic.
	SubjectGetDBBootstrapConfig = "rds.GetDBBootstrapConfig"

	// Layer-1 customer action subjects, answered by whichever daemon the queue
	// group picks — a create does its own orchestration inline.
	SubjectCreateDBInstance    = "rds.CreateDBInstance"
	SubjectDescribeDBInstances = "rds.DescribeDBInstances"

	// Lifecycle actions. Each drives the VM and the in-guest engine inline, so
	// the daemon answering one is doing the work rather than queueing it.
	SubjectRebootDBInstance = "rds.RebootDBInstance"
	SubjectStartDBInstance  = "rds.StartDBInstance"
	SubjectStopDBInstance   = "rds.StopDBInstance"
	SubjectDeleteDBInstance = "rds.DeleteDBInstance"
	SubjectModifyDBInstance = "rds.ModifyDBInstance"

	SubjectDescribeEvents = "rds.DescribeEvents"

	// Backup and recovery. A create quiesces the engine and a restore launches a
	// VM, so both are as much orchestration as the lifecycle actions above.
	SubjectCreateDBSnapshot                = "rds.CreateDBSnapshot"
	SubjectDescribeDBSnapshots             = "rds.DescribeDBSnapshots"
	SubjectDeleteDBSnapshot                = "rds.DeleteDBSnapshot"
	SubjectRestoreDBInstanceFromDBSnapshot = "rds.RestoreDBInstanceFromDBSnapshot"

	// The automated-backup set, which is a KV read: the snapshots themselves are
	// listed through DescribeDBSnapshots.
	SubjectDescribeDBInstanceAutomatedBackups = "rds.DescribeDBInstanceAutomatedBackups"

	// Configuration resources. Plain KV CRUD, so unlike the instance actions
	// none of them drives a VM and all take the default budget.
	SubjectCreateDBSubnetGroup    = "rds.CreateDBSubnetGroup"
	SubjectDescribeDBSubnetGroups = "rds.DescribeDBSubnetGroups"
	SubjectDeleteDBSubnetGroup    = "rds.DeleteDBSubnetGroup"

	SubjectCreateDBParameterGroup    = "rds.CreateDBParameterGroup"
	SubjectDescribeDBParameterGroups = "rds.DescribeDBParameterGroups"
	SubjectModifyDBParameterGroup    = "rds.ModifyDBParameterGroup"
	SubjectDescribeDBParameters      = "rds.DescribeDBParameters"
	SubjectDeleteDBParameterGroup    = "rds.DeleteDBParameterGroup"

	// Tag actions address the resource by ARN, so one subject serves every
	// resource type rather than one per kind.
	SubjectAddTagsToResource      = "rds.AddTagsToResource"
	SubjectRemoveTagsFromResource = "rds.RemoveTagsFromResource"
	SubjectListTagsForResource    = "rds.ListTagsForResource"

	// Queue groups are scoped per subject, so one name shares command delivery
	// across gateway nodes for the whole fleet.
	CommandQueueGroup = "spinifex-rds-agents"
)

func BusRegisterSubject(accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("%s%s.%s.register", busPrefix, accountID, dbInstanceIdentifier)
}

// The reserved .heartbeat subject stays unused: the beat is folded into the
// state change so a healthy instance costs one round trip per tick.
func BusHealthSubject(accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("%s%s.%s.health", busPrefix, accountID, dbInstanceIdentifier)
}

// A live long poll rather than a durable queue, so a set-password that cannot
// reach the agent fails loudly instead of deferring cleartext.
func BusCommandSubject(accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("%s%s.%s.command", busPrefix, accountID, dbInstanceIdentifier)
}

// Correlated by the issuer on CommandID.
func BusCommandReplySubject(accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("%s%s.%s.command-reply", busPrefix, accountID, dbInstanceIdentifier)
}
