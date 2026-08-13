// Package networkconnections embeds the inbound-listener inventory doc that
// lives alongside it, so test binaries can read it on nodes with no Go
// toolchain. //go:embed cannot reach outside its own directory.
package networkconnections

import _ "embed"

//go:embed README.md
var readme []byte

// README returns the raw bytes of the inbound-listener inventory doc
// (docs/security/network-connections/README.md), embedded at compile time.
func README() []byte {
	return readme
}
