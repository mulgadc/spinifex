// Package networkv1 is the versioned wire vocabulary for the vpc.*
// control-plane-to-network-realisation boundary. See README.md for the
// route table, ownership statement and compatibility rules.
package networkv1

// QueueGroup is the NATS queue group every vpcd subscribes under, so
// exactly one vpcd instance processes a given event.
const QueueGroup = "vpcd-workers"
