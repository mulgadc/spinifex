// Package listenerinventory parses the "## 1. Inbound Listeners" table in
// docs/security/network-connections/README.md into a queryable Table of
// port -> scope -> exception-declaring prose.
//
// It exists so the inventory doc is the single fixture two otherwise
// unrelated checks read: the static bind-site scan in
// spinifex/network/invariants, which reads install scripts and config
// templates, and the runtime e2e check in tests/e2e/multinode, which reads
// `ss -tulnp` on live nodes. Neither belongs under spinifex/network/'s
// ADR-0006 layer tree — this package is not part of that stack, and lives
// beside invariants for the same reason invariants does: it is read by both
// a go-test-only consumer and an e2e-tagged one, so it cannot itself carry
// a build tag or live under either caller.
package listenerinventory
