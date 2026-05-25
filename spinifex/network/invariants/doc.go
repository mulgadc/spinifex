// Package invariants holds executable tests that pin the spinifex network
// layer tree to the safety and liveness invariants declared in ADR-0006
// (docs/development/proposals/az-local-architecture/0006-spinifex-network-layer-contract.md).
//
// Each test name encodes its governing ADR clause ID (TestS1_…, TestS4_…,
// TestL1_…). On failure the test quotes the ADR clause verbatim so the fix
// path leads back to the contract, not to the test author.
//
// The package contains no production code; it exists solely so `go test
// ./spinifex/network/invariants/...` runs the suite without polluting any
// layer's own package.
//
// See docs/development/improvements/network-invariants.md for the
// per-clause mechanic and phasing.
package invariants
