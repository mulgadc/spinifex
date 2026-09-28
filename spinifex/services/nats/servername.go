package nats

import "strings"

// ServerNamePrefix is what nats.conf prefixes a node name with to build
// server_name. JetStream reports stream peers by server name, so anything
// mapping a replica back to the node holding it has to go through here rather
// than assume the two are the same string — they are not.
const ServerNamePrefix = "spinifex-nats-"

// ServerName is the NATS server_name for a node in this cluster.
func ServerName(node string) string { return ServerNamePrefix + node }

// NodeFromServerName is ServerName's inverse. The second result is false for a
// name this cluster did not generate, so a caller can tell "not our node" from
// "a node called the empty string".
func NodeFromServerName(server string) (string, bool) {
	return strings.CutPrefix(server, ServerNamePrefix)
}
