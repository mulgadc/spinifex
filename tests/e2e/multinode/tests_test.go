//go:build e2e

package multinode

import "testing"

// Top-level Test* wrappers. Each delegates to a runX function in the matching
// <name>_test.go file. Names follow the single-node convention (TestX) so
// isolated runs via `go test -run TestMultinodeClusterHealth` are stable.
//
// Spread placement + NAT GW (formerly bash phase 11) lives in
// placement_nat_test.go as 6 top-level TestMultinodeSpread* funcs after the
// mulga-siv-127 flatten — no wrappers needed for those.
//
// Parallelism (mulga-siv-127 Stage J):
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
//   - TestMultinodeCrossNodeOps          : stops/starts trio[0], would race
//     TestMultinodeGuestSSH which iterates every trio member. Bead spec
//     listed it in bucket #1 but the trio mutation makes that unsafe;
//     keep sequential until bucket #3 reworks shared-state ownership.
//   - TestMultinodeNodeFailure/Recovery  : StopNode/StartNode mutate cluster.
//   - TestMultinodeSpread*               : sequential vs each other due to
//     EIP pool contention + shared VPC CIDR 10.100.0.0/16.

// TestMultinodePreflight runs sequentially because it initialises the
// package fixture singleton.
func TestMultinodePreflight(t *testing.T) {
	runPreflight(t, requireMultiNodeFixture(t))
}

func TestMultinodeClusterHealth(t *testing.T) {
	t.Parallel()
	runClusterHealth(t, requireMultiNodeFixture(t))
}

func TestMultinodeInstanceDistribution(t *testing.T) {
	t.Parallel()
	runInstanceDistribution(t, requireMultiNodeFixture(t))
}

func TestMultinodeGuestSSH(t *testing.T) {
	t.Parallel()
	runGuestSSH(t, requireMultiNodeFixture(t))
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

// TestMultinodeCrossNodeOps is sequential — stops/starts trio[0], which
// would race TestMultinodeGuestSSH.
func TestMultinodeCrossNodeOps(t *testing.T) {
	runCrossNodeOps(t, requireMultiNodeFixture(t))
}

func TestMultinodeNodeFailure(t *testing.T) {
	runNodeFailure(t, requireMultiNodeFixture(t))
}

func TestMultinodeNodeRecovery(t *testing.T) {
	runNodeRecovery(t, requireMultiNodeFixture(t))
}

// TestMultinodeVPCNetworking owns its own 10.200.0.0/16 VPC (no EIP use)
// so it's safe alongside bucket #1.
func TestMultinodeVPCNetworking(t *testing.T) {
	t.Parallel()
	runVPCNetworking(t, requireMultiNodeFixture(t))
}
