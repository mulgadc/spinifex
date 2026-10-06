// Package listenerinventory parses the "## 1. Inbound Listeners" table in
// docs/security/network-connections/README.md into a queryable Table of
// port -> scope -> exception-declaring prose.
//
// It exists so the inventory doc is the single fixture two otherwise
// unrelated checks read: the static bind-site scan in
// spinifex/domains/network/invariants, which reads install scripts and config
// templates, and the runtime e2e check in tests/e2e/multinode, which reads
// `ss -tulnp` on live nodes. Neither belongs under the executable network
// layer tree — this package is not part of that stack. It lives in the
// internal test kit because both a go-test-only consumer and an e2e-tagged
// consumer read it, so it cannot itself carry either caller's build tag.
package listenerinventory
