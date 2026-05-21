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
//   - DHCP-sourced pools (source="dhcp") use upstream router DHCP to acquire
//     the gateway LRP IP. The DHCP client and JetStream lease store live in
//     services/vpcd/dhcp; this package keeps the dependency out by accepting
//     a GatewayIPAllocator interface. StaticRangeAllocator is provided here;
//     the DHCP-backed allocator stays in vpcd until bead mulga-siv-125.3.3
//     (DHCP removal) is unblocked.
//
// See docs/development/feature/spinifex-network-redesign.md §10 and
// docs/development/feature/spinifex-network-redesign-phase2.md §2.4.
package external
