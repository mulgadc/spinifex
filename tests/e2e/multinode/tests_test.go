//go:build e2e

package multinode

import (
	"testing"
)

// Top-level Test* wrappers, each delegating to a runX function. Parallel tests
// share the read-only singleton trio; sequential tests mutate cluster state
// (stop/start nodes, predastore, EIP pool) and are ordered so the cluster is
// fully restabilised before GuestSSH probes every trio member.

// TestMultinodePreflight runs sequentially first to initialise the package fixture singleton.
func TestMultinodePreflight(t *testing.T) {
	runPreflight(t, requireMultiNodeFixture(t))
}

// Baseline tests run before needInstanceTrio so the default SG/subnet/route table
// are in pristine state. Both own dedicated SGs and self-cleaning instances.
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

// TestMultinodeDNS is sequential because it launches guests and briefly stops
// one Northstar unit while exercising resolver failover.
func TestMultinodeDNS(t *testing.T) {
	runMultinodeDNS(t, requireMultiNodeFixture(t))
}

// TestMultinodeJetStreamReplicas is read-only over NATS; parallel so it resumes
// after the sequential node-failure/recovery tests have restabilised the cluster.
func TestMultinodeJetStreamReplicas(t *testing.T) {
	t.Parallel()
	runJetStreamReplicas(t, requireMultiNodeFixture(t))
}

// TestMultinodeVolumeLifecycle is sequential: touches predastore state.
func TestMultinodeVolumeLifecycle(t *testing.T) {
	runVolumeLifecycle(t, requireMultiNodeFixture(t))
}

// TestMultinodeVolumeDurability is sequential and declared before CrossNodeOps
// (which destabilises trio[0]'s sshd) so its guest-SSH probes hit settled VMs.
// Touches predastore state.
func TestMultinodeVolumeDurability(t *testing.T) {
	runVolumeDurability(t, requireMultiNodeFixture(t))
}

// TestMultinodeCrossNodeGateway is sequential: asserts a stable per-gateway instance count
// that concurrent launches/terminates would break.
func TestMultinodeCrossNodeGateway(t *testing.T) {
	runCrossNodeGateway(t, requireMultiNodeFixture(t))
}

// TestMultinodeCrossNodeOps is sequential: stops/starts trio[0]; GuestSSH is declared
// after this so it probes the trio only after the cluster has restabilised.
func TestMultinodeCrossNodeOps(t *testing.T) {
	runCrossNodeOps(t, requireMultiNodeFixture(t))
}

func TestMultinodeNodeFailure(t *testing.T) {
	runNodeFailure(t, requireMultiNodeFixture(t))
}

func TestMultinodeNodeRecovery(t *testing.T) {
	runNodeRecovery(t, requireMultiNodeFixture(t))
}

// TestMultinodeOVNRaft is sequential: stops ovn-central on the NB leader to
// prove DB failover, restoring it in cleanup before later tests run.
func TestMultinodeOVNRaft(t *testing.T) {
	runOVNRaft(t, requireMultiNodeFixture(t))
}

// TestMultinodeSpread is sequential after NodeRecovery; owns 10.100.0.0/16 + EIP pool.
func TestMultinodeSpread(t *testing.T) {
	runSpread(t, requireMultiNodeFixture(t))
}

// TestMultinodeGuestSSH is sequential and declared last so it runs after the cluster has
// fully restabilised. CrossNodeOps stops/starts trio[0] and only waits for "running" — not
// sshd — so GuestSSH must not probe until the guest has settled.
func TestMultinodeGuestSSH(t *testing.T) {
	runGuestSSH(t, requireMultiNodeFixture(t))
}

// TestMultinodeVPCNetworking owns 10.200.0.0/16 (no EIP use); safe to run in parallel.
func TestMultinodeVPCNetworking(t *testing.T) {
	t.Parallel()
	runVPCNetworking(t, requireMultiNodeFixture(t))
}

// TestMultinodeIPSec is read-only over SSH; launches no AWS resources.
func TestMultinodeIPSec(t *testing.T) {
	t.Parallel()
	runIPSec(t, requireMultiNodeFixture(t))
}
