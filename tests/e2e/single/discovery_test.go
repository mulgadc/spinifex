//go:build e2e

package single

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runEnvironment verifies KVM, AWS gateway TLS reachability, daemon NATS
// readiness, and the basic region/AZ discovery calls. Maps to run-e2e.sh
// lines ~51–93.
func runEnvironment(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Environment")

	// /dev/kvm — skip rather than fatal in local-dev to match the bash
	// "exit 1" only when KVM is genuinely required (CI). Local devs without
	// nested virt would otherwise be unable to even compile-run this suite.
	harness.Step(t, "checking /dev/kvm")
	st, err := os.Stat("/dev/kvm")
	if err != nil {
		t.Skipf("/dev/kvm not present (%v); single-node suite needs KVM", err)
	}
	if st.Mode()&0o200 == 0 {
		// Bash treated non-writable as fatal — replicate.
		t.Fatalf("/dev/kvm exists but is not writable (mode=%v)", st.Mode())
	}
	harness.Detail(t, "kvm", "writable")

	// AWS gateway TLS handshake. Bash does `curl -sk` so it accepts any
	// non-zero status as success; we use HTTPSGet with a nil pool so the
	// system trust store is used (the cert suite already validates CA
	// installation), and treat a non-zero HTTP status as a successful
	// handshake.
	harness.Step(t, "waiting for AWS gateway")
	host := "127.0.0.1"
	if len(fix.Env.ServiceIPs) > 0 {
		host = fix.Env.ServiceIPs[0]
	}
	gwURL := fmt.Sprintf("https://%s:%d/", host, fix.Env.AWSGWPort)
	harness.Eventually(t, func() bool {
		code, _, err := harness.HTTPSGet(gwURL, nil, fix.Env.DefaultTimeout)
		return err == nil && code != 0
	}, 30*time.Second, 1*time.Second, "AWS gateway not reachable at "+gwURL)
	harness.Detail(t, "gateway", gwURL)

	// Daemon NATS readiness: bash polls describe-instance-types via the
	// AWS CLI until it returns a non-empty list. Replicate via the SDK so
	// we exercise the same NATS subscription path.
	harness.Step(t, "waiting for daemon NATS subscriptions")
	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
		if err != nil {
			return err
		}
		if len(out.InstanceTypes) == 0 {
			return errors.New("describe-instance-types: empty result")
		}
		return nil
	}, 30*time.Second, 1*time.Second)

	// Region + AZ smoke calls.
	harness.Step(t, "describe-regions / describe-availability-zones")
	regions, err := fix.AWS.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions")
	require.NotEmpty(t, regions.Regions, "no regions returned")

	azOut, err := fix.AWS.EC2.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{})
	require.NoError(t, err, "describe-availability-zones")
	require.NotEmpty(t, azOut.AvailabilityZones, "no AZs returned")
	azName := aws.StringValue(azOut.AvailabilityZones[0].ZoneName)
	azState := aws.StringValue(azOut.AvailabilityZones[0].State)
	require.Equalf(t, "available", azState, "AZ %s state %q (want available)", azName, azState)
	harness.Detail(t, "az", azName, "region", aws.StringValue(azOut.AvailabilityZones[0].RegionName))

	harness.OnFailure(t, func() {
		harness.DumpCmd(t, fix.Artifacts, "phase1-az.txt",
			"aws", "ec2", "describe-availability-zones")
	})
}

// runClusterStatsCLI exercises the spx CLI cluster surface (`get nodes`,
// `top nodes`, `get vms`). Single-node only — multinode mode is tested by a
// different scenario. Maps to run-e2e.sh ~95–127.
func runClusterStatsCLI(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Cluster Stats CLI")
	if fix.Env.Mode != harness.ModeSingle {
		t.Skipf("Phase 1b is single-node only (mode=%s)", fix.Env.Mode)
	}

	harness.Step(t, "spx get nodes")
	nodes := harness.SpxGetNodes(t)
	assert.Contains(t, nodes, "Ready", "spx get nodes should report a Ready node\n%s", nodes)

	harness.Step(t, "spx top nodes")
	top := harness.SpxTopNodes(t)
	// Bash matches `0/` in the resource stat column (e.g. "0/4 vCPU"). The
	// presence of that fraction marker is what proves the stats column
	// actually rendered.
	assert.Contains(t, top, "0/", "spx top nodes should report resource stats\n%s", top)

	harness.Step(t, "spx get vms (empty before any launches)")
	vms := harness.SpxGetVMs(t)
	assert.Contains(t, vms, "No VMs found",
		"spx get vms should be empty before Phase 5\n%s", vms)
}

// runDiscovery re-asserts describe-regions / describe-availability-zones
// (cheap but it's where the bash records them as a phase boundary) and picks
// the nano instance type + architecture used throughout the rest of the run.
// Maps to run-e2e.sh ~128–164.
func runDiscovery(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Discovery Phase 2 — Discovery & Metadata Metadata")

	regions, err := fix.AWS.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err)
	require.NotEmpty(t, regions.Regions, "describe-regions empty")

	azOut, err := fix.AWS.EC2.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{})
	require.NoError(t, err)
	require.NotEmpty(t, azOut.AvailabilityZones, "describe-availability-zones empty")

	harness.Step(t, "discovering nano instance type")
	instType, arch := needInstanceTypeArch(t, fix)
	require.NotEmpty(t, instType, "no nano instance type discovered")
	require.NotEmpty(t, arch, "nano instance type missing SupportedArchitectures")
	az := needAZ(t, fix)
	harness.Detail(t, "instance_type", instType, "arch", arch, "az", az)
}

// runSerialConsoleAccess flips serial-console-access on then off and verifies
// each transition with both the action-returned bool and a follow-up
// get-status round-trip. Maps to run-e2e.sh ~166–202.
func runSerialConsoleAccess(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Serial Console Access")

	harness.Step(t, "default state should be disabled")
	if got := harness.GetSerialConsoleAccessEnabled(t, fix.AWS); got {
		t.Fatalf("expected serial console default disabled, got enabled")
	}

	harness.Step(t, "enable")
	if got := harness.EnableSerialConsoleAccess(t, fix.AWS); !got {
		t.Fatalf("enable: action returned enabled=false")
	}
	if got := harness.GetSerialConsoleAccessEnabled(t, fix.AWS); !got {
		t.Fatalf("enable: subsequent get-status returned disabled")
	}
	harness.Detail(t, "state", "enabled")

	harness.Step(t, "disable")
	if got := harness.DisableSerialConsoleAccess(t, fix.AWS); got {
		t.Fatalf("disable: action returned enabled=true")
	}
	if got := harness.GetSerialConsoleAccessEnabled(t, fix.AWS); got {
		t.Fatalf("disable: subsequent get-status returned enabled")
	}
	harness.Detail(t, "state", "disabled")
}
