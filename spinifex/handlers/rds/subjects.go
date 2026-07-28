package handlers_rds

import "fmt"

// Layer-2 bus subjects. The agent never publishes to these itself — the gateway
// relays its SigV4 calls onto them, keeping NATS host-internal.
//
// Instance identity on the bus is {accountID}.{dbInstanceIdentifier}, matching
// the KV layout and deliberately not the internal EC2 instance ID, which changes
// on every VM replace while the subject must not.
const (
	busPrefix = "rds.bus."

	// SubjectRegisterWildcard and friends are what the daemon subscribes on; the
	// account and DB segments are addressing, and the payload is authoritative.
	SubjectRegisterWildcard = busPrefix + "*.*.register"
	SubjectHealthWildcard   = busPrefix + "*.*.health"

	// SubjectGetDBBootstrapConfig is a Layer-1 subject, not a bus one: serving
	// bootstrap config is a control-plane read rather than reconciler↔agent
	// traffic.
	SubjectGetDBBootstrapConfig = "rds.GetDBBootstrapConfig"

	// CommandQueueGroup shares command delivery across gateway nodes. Queue
	// groups are scoped per subject, so one name is enough for the whole fleet.
	CommandQueueGroup = "spinifex-rds-agents"
)

// BusRegisterSubject is the agent's boot-time registration subject.
func BusRegisterSubject(accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("%s%s.%s.register", busPrefix, accountID, dbInstanceIdentifier)
}

// BusHealthSubject carries the periodic state-and-liveness beat. The reserved
// .heartbeat subject stays unused: the beat is folded into the state change so a
// healthy instance costs one round trip per tick.
func BusHealthSubject(accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("%s%s.%s.health", busPrefix, accountID, dbInstanceIdentifier)
}

// BusCommandSubject is the reconciler → agent directive channel. It is a live
// long poll rather than a durable queue, so a set-password that cannot reach
// the agent fails loudly instead of deferring cleartext.
func BusCommandSubject(accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("%s%s.%s.command", busPrefix, accountID, dbInstanceIdentifier)
}

// BusCommandReplySubject carries the agent's result back to the issuer, which
// correlates it by CommandID.
func BusCommandReplySubject(accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("%s%s.%s.command-reply", busPrefix, accountID, dbInstanceIdentifier)
}
