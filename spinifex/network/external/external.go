// Package external is L5 of the spinifex network stack: VPC-facing external
// connectivity — Internet Gateway, Elastic IP, and NAT Gateway. L5 sits on
// top of L1 (network/ovn) for the logical objects an IGW needs, L3
// (network/policy) for NAT rules and routes, and (transitively) L0
// (network/host) for the uplink CIDR resolved at startup.
//
// Layering notes for this package (Phase 2.4):
//
//   - The parent plan (§10.1) calls for L5 to drive L2 (network/topology) for
//     external switch / localnet port / gateway LRP. Until topology.Manager
//     grows those methods, IGWManager calls L1 directly for those L2 objects.
//     This is documented and tracked for cleanup in a follow-on bead.
//
//   - Gateway LRP IP allocation goes through a GatewayIPAllocator interface.
//     StaticRangeAllocator (this package) picks from pool.gw_lrp_range;
//     LinkLocalAllocator returns ok=false for distributed-NAT deployments
//     where the gateway LRP never goes on the wire.
//
// See docs/development/feature/spinifex-network-redesign.md §10 and
// docs/development/feature/spinifex-network-redesign-phase2.md §2.4.
package external
