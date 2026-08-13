// Package networkconnections embeds the inbound-listener inventory doc that
// lives alongside it. //go:embed cannot reach outside its own directory, so
// this package exists to give that traversal a compile-time home: callers
// get the doc's bytes without shelling out to a Go toolchain at runtime,
// which matters for test binaries that run on nodes with no toolchain
// installed.
package networkconnections

import _ "embed"

//go:embed README.md
var readme []byte

// README returns the raw bytes of the inbound-listener inventory doc
// (docs/security/network-connections/README.md), embedded at compile time.
func README() []byte {
	return readme
}
