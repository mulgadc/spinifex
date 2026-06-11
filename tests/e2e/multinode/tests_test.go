//go:build e2e

package multinode

import "testing"

// Top-level Test* wrappers. Each delegates to a runX function in the matching
// <name>_test.go file. Names follow the single-node convention (TestX) so
// isolated runs via `go test -run TestMultinodeClusterHealth` are stable.
//
// Spread placement + NAT GW (formerly bash phase 11) lives in
// placement_nat_test.go as a single TestMultinodeSpread with 6 t.Run
// sub-tests sharing one VPC + bastion + private trio + NAT GW setup chain.
// Sub-test layout keeps JUnit granularity without paying 6× setup cost.
//
// Parallelism:
//
// Bucket #1 (parallel): the read-only / independent Tests below call
// t.Parallel(). They share the package-singleton trio (sync.Once gated) and
// the harness Fixture but never mutate it — DescribeInstances / SSH probe /
// VPC creation in independent CIDR space.
//
// Sequential (no t.Parallel — would race the parallel bucket):
//   - TestMultinodePreflight             : runs first; initialises pkg fixture.
//   - TestMultinodeVolumeLifecycle       : touches predastore state.
//   - TestMultinodeCrossNodeGateway      : asserts equality between baseline
//     DescribeInstances and per-gateway DescribeInstances; concurrent VPC
//     test launches/terminates instances mid-assert, breaking equality.
//     Bead spec listed it in bucket #1 but the snapshot assumption fails
//     under parallel state churn; keep sequential.
//   - TestMultinodeCrossNodeOps          : stops/starts trio[0]. Only waits
//     for state "running", not for sshd; GuestSSH must not probe the trio
//     until the guest has settled. Bead spec listed it in bucket #1 but the
//     trio mutation makes that unsafe; keep sequential.
//   - TestMultinodeNodeFailure/Recovery  : StopNode/StartNode mutate cluster.
//   - TestMultinodeSpread                : owns EIP pool + VPC CIDR
//     10.100.0.0/16; sub-tests share the setup chain sequentially.
//   - TestMultinodeGuestSSH              : iterates every trio member over
//     SSH. Declared last so it runs after CrossNodeOps + NodeFailure/Recovery
//     + Spread have restabilised the cluster — the restarted trio[0] is then
//     long-booted and there is no concurrent VM churn to delay sshd.

// TestMultinodePreflight runs sequentially because it initialises the
// package fixture singleton.
func TestMultinodePreflight(t *testing.T) {
	runPreflight(t, requireMultiNodeFixture(t))
}

// Fresh-install reachability baselines run right after preflight and before
// any test calls needInstanceTrio (which authorizes SSH on the default SG),
// so the default SG / subnet / route table are exercised pristine. Both own
// their mutable resources (dedicated SGs, self-cleaning instances) and never
// mutate a default resource.
func TestMultinodeDefaultSGReachabilityBaseline(t *testing.T) {
	runMultinodeDefaultSGReachabilityBaseline(t, requireMultiNodeFixture(t))
}

func TestMultinodeSameSGCrossHostComms(t *testing.T) {
	runMultinodeSameSGCrossHostComms(t, requireMultiNodeFixture(t))
}

func TestMultinodeClusterHealth(t *testing.T) {
	t.Parallel()
	runClusterHealth(t, requireMultiNodeFixture(t))
}

func TestMultinodeInstanceDistribution(t *testing.T) {
	t.Parallel()
	runInstanceDistribution(t, requireMultiNodeFixture(t))
}

// TestMultinodeVolumeLifecycle is sequential — touches predastore state
// shared with other suites.
func TestMultinodeVolumeLifecycle(t *testing.T) {
	runVolumeLifecycle(t, requireMultiNodeFixture(t))
}

// TestMultinodeCrossNodeGateway is sequential — asserts a stable
// instance-count snapshot across gateways, which the parallel VPC test
// would break by launching/terminating its own instances mid-assert.
func TestMultinodeCrossNodeGateway(t *testing.T) {
	runCrossNodeGateway(t, requireMultiNodeFixture(t))
}

// TestMultinodeCrossNodeOps is sequential — stops/starts trio[0]. It waits
// only for state "running", so GuestSSH (declared last) probes the trio after
// the cluster restabilises rather than racing this restart.
func TestMultinodeCrossNodeOps(t *testing.T) {
	runCrossNodeOps(t, requireMultiNodeFixture(t))
}

func TestMultinodeNodeFailure(t *testing.T) {
	runNodeFailure(t, requireMultiNodeFixture(t))
}

func TestMultinodeNodeRecovery(t *testing.T) {
	runNodeRecovery(t, requireMultiNodeFixture(t))
}

// TestMultinodeSpread runs after NodeRecovery so the cluster is fully
// healthy + degrade-tested before this Test launches its 4-VM + NAT GW +
// custom-VPC graph. Sequential — owns 10.100.0.0/16 + EIP pool; sub-tests
// share the setup chain (see placement_nat_test.go).
func TestMultinodeSpread(t *testing.T) {
	runSpread(t, requireMultiNodeFixture(t))
}

// TestMultinodeGuestSSH is sequential and declared last in the sequential
// bucket so it runs after the cluster has fully restabilised. It iterates
// every trio member, and CrossNodeOps stop/starts trio[0] — which only waits
// for state "running", not for sshd to answer. Running parallel, GuestSSH
// resumed immediately after CrossNodeOps and probed a guest still booting,
// yielding kex_exchange_identification resets and 2m timeouts. Ordering it
// after NodeFailure/Recovery + Spread gives every trio member ample settle
// time and removes concurrent VM churn from the parallel bucket.
func TestMultinodeGuestSSH(t *testing.T) {
	runGuestSSH(t, requireMultiNodeFixture(t))
}

// TestMultinodeVPCNetworking owns its own 10.200.0.0/16 VPC (no EIP use)
// so it's safe alongside bucket #1.
func TestMultinodeVPCNetworking(t *testing.T) {
	t.Parallel()
	runVPCNetworking(t, requireMultiNodeFixture(t))
}

// TestMultinodeIPSec is read-only over SSH — verifies the OVN native IPsec
// wiring (OVS DB cert pointers, xfrm SAs, ESP traffic) without launching
// any AWS resources. Safe to run alongside the parallel bucket.
func TestMultinodeIPSec(t *testing.T) {
	t.Parallel()
	runIPSec(t, requireMultiNodeFixture(t))
}
