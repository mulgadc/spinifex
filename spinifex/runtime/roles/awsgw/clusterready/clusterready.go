// Package clusterready reports whether the message bus the AWS services depend on
// is reachable, so the request pipeline can refuse early instead of waiting out
// every per-call timeout.
package clusterready

import "github.com/nats-io/nats.go"

// Bus reports readiness from a NATS connection.
type Bus struct{ conn *nats.Conn }

// New returns the readiness of conn. A nil conn is never available.
func New(conn *nats.Conn) *Bus { return &Bus{conn: conn} }

// Available reports whether the connection exists and is connected.
func (b *Bus) Available() bool {
	return b != nil && b.conn != nil && b.conn.IsConnected()
}
