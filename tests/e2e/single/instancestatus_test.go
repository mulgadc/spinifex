//go:build e2e

package single

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DescribeInstanceStatus vocabulary, duplicated from the handler rather than
// imported: the test asserts the wire contract an AWS SDK client sees, so it
// must fail if the handler's constants change.
const (
	instStatusOK            = "ok"
	instStatusPassed        = "passed"
	instStatusInitializing  = "initializing"
	instStatusNotApplicable = "not-applicable"
	instStatusReachability  = "reachability"
)

// instStatusGraceProbe bounds how long after RunInstances the initializing
// assertion stays a hard requirement. The handler reports initializing for two
// minutes from LaunchTime, so a probe inside this window is inside the grace by
// construction; past it a loaded runner may legitimately have crossed into ok
// and a hard failure would be flaky for a reason unrelated to the contract.
const instStatusGraceProbe = 90 * time.Second

// runInstanceStatus drives DescribeInstanceStatus end to end against a guest
// this test launches and owns. It is sequential: it stops and terminates that
// guest, and the initializing -> ok transition is only observable on a freshly
// launched VM, so the long-booted singleton cannot stand in for it.
//
// The valuable assertion is stage 2 — the real SDK waiter is exactly what
// `aws ec2 wait instance-status-ok` and Terraform's aws_instance creation poll
// run, so a malformed status envelope surfaces here as a hang, not a failed
// call. The remaining stages cover the paths a mock cannot express: the
// running-only state gate, the stopped-instance KV augmentation behind
// IncludeAllInstances, gateway-side dedup, and the status-specific filter map.
func runInstanceStatus(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — DescribeInstanceStatus + SDK status waiter")

	instType, _ := needInstanceTypeArch(t, fix)
	keyName, _ := needKeyPair(t, fix)
	amiID := needAMI(t, fix)
	az := needAZ(t, fix)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SGID, "default SG ID required")

	harness.Step(t, "run-instances ami=%s type=%s (test-owned guest)", amiID, instType)
	launchedAt := time.Now()
	runOut, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{ // e2e:allow-create — the initializing→ok transition needs a guest launched by this test, not the long-booted singleton.
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(vpc.SubnetID),
		SecurityGroupIds: []*string{aws.String(vpc.SGID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoError(t, err, "run-instances")
	require.NotEmpty(t, runOut.Instances, "run-instances returned no Instances")
	instanceID := aws.StringValue(runOut.Instances[0].InstanceId)
	require.NotEmpty(t, instanceID, "run-instances returned empty InstanceId")
	harness.Detail(t, "instance", instanceID)

	// Registered before the first blocking wait so a fatal anywhere below still
	// tears the guest down; the stage-8 terminate makes this a no-op on success.
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
	})

	harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")

	// --- Stage 1: a fresh launch reports initializing ---

	harness.Step(t, "describe-instance-status %s (fresh launch)", instanceID)
	fresh := requireInstanceStatus(t, fix, instanceID,
		&ec2.DescribeInstanceStatusInput{InstanceIds: []*string{aws.String(instanceID)}})
	freshStatus := aws.StringValue(fresh.InstanceStatus.Status)
	sinceLaunch := time.Since(launchedAt)
	harness.Detail(t, "fresh_status", freshStatus, "since_launch", sinceLaunch.Round(time.Second))

	if sinceLaunch < instStatusGraceProbe {
		require.Equalf(t, instStatusInitializing, freshStatus,
			"guest %s described %s after launch must still be inside the initializing grace", instanceID, sinceLaunch)
		requireReachability(t, fresh.InstanceStatus, instStatusInitializing)
	} else {
		// Boot outran the probe window on a loaded runner: ok is then a correct
		// answer too, so record which was seen rather than failing on timing.
		require.Containsf(t, []string{instStatusInitializing, instStatusOK}, freshStatus,
			"fresh guest %s reported unexpected InstanceStatus %q", instanceID, freshStatus)
		t.Logf("fresh-launch probe ran %s after RunInstances (grace is 2m); observed InstanceStatus=%q",
			sinceLaunch.Round(time.Second), freshStatus)
	}

	// --- Stage 2: the real SDK waiter terminates ---

	harness.Step(t, "wait instance-status-ok %s (SDK waiter)", instanceID)
	waiterStart := time.Now()
	require.NoErrorf(t, fix.AWS.EC2.WaitUntilInstanceStatusOk(&ec2.DescribeInstanceStatusInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}), "wait instance-status-ok %s", instanceID)
	harness.Detail(t, "waiter_elapsed", time.Since(waiterStart).Round(time.Second))

	// --- Stage 3: steady-state envelope ---

	harness.Step(t, "describe-instance-status %s (steady state)", instanceID)
	steady := requireInstanceStatus(t, fix, instanceID,
		&ec2.DescribeInstanceStatusInput{InstanceIds: []*string{aws.String(instanceID)}})
	assert.Equal(t, instStatusOK, aws.StringValue(steady.InstanceStatus.Status), "InstanceStatus.Status")
	instDetail := requireReachability(t, steady.InstanceStatus, instStatusPassed)
	// ImpairedSince is only stamped once QMP health crosses the failure gate.
	assert.Nil(t, instDetail.ImpairedSince, "healthy guest must not carry ImpairedSince")
	assert.Equal(t, instStatusOK, aws.StringValue(steady.SystemStatus.Status), "SystemStatus.Status")
	requireReachability(t, steady.SystemStatus, instStatusPassed)
	require.NotNil(t, steady.InstanceState, "InstanceState missing")
	assert.Equal(t, "running", aws.StringValue(steady.InstanceState.Name), "InstanceState.Name")
	assert.Equal(t, int64(16), aws.Int64Value(steady.InstanceState.Code), "InstanceState.Code")
	assert.Equal(t, az, aws.StringValue(steady.AvailabilityZone), "AvailabilityZone")

	// --- Stage 4: the default describe excludes non-running instances ---

	harness.Step(t, "stop-instances %s", instanceID)
	_, err = fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "stop-instances %s", instanceID)
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")

	// The node handler drops the VM from its running-only view asynchronously
	// with the KV write that DescribeInstances reads, so poll for absence
	// rather than assuming the two flip together.
	harness.Step(t, "describe-instance-status %s (default: running only)", instanceID)
	harness.EventuallyErr(t, func() error {
		statuses := describeInstanceStatuses(t, fix,
			&ec2.DescribeInstanceStatusInput{InstanceIds: []*string{aws.String(instanceID)}})
		if s := findInstanceStatus(statuses, instanceID); s != nil {
			return fmt.Errorf("stopped guest %s still returned by the default describe (state=%s)",
				instanceID, aws.StringValue(s.InstanceState.Name))
		}
		return nil
	}, 60*time.Second, 2*time.Second)

	// --- Stage 5: IncludeAllInstances surfaces the stopped guest from the KV path ---

	harness.Step(t, "describe-instance-status %s --include-all-instances", instanceID)
	stopped := requireInstanceStatus(t, fix, instanceID, &ec2.DescribeInstanceStatusInput{
		InstanceIds:         []*string{aws.String(instanceID)},
		IncludeAllInstances: aws.Bool(true),
	})
	require.NotNil(t, stopped.InstanceState, "InstanceState missing")
	assert.Equal(t, "stopped", aws.StringValue(stopped.InstanceState.Name), "InstanceState.Name")
	assert.Equal(t, instStatusNotApplicable, aws.StringValue(stopped.InstanceStatus.Status), "InstanceStatus.Status")
	requireReachability(t, stopped.InstanceStatus, instStatusNotApplicable)
	assert.Equal(t, instStatusNotApplicable, aws.StringValue(stopped.SystemStatus.Status), "SystemStatus.Status")
	requireReachability(t, stopped.SystemStatus, instStatusNotApplicable)

	// --- Stage 6: no duplicates across the unscoped fan-out + KV union ---

	// describeInstanceStatuses fails on any repeated id, so an unscoped
	// IncludeAllInstances describe — where the node fan-out and the stopped-KV
	// augmentation both contribute — is the widest check on gateway dedup.
	harness.Step(t, "describe-instance-status --include-all-instances (unscoped, dedup)")
	unscoped := describeInstanceStatuses(t, fix,
		&ec2.DescribeInstanceStatusInput{IncludeAllInstances: aws.Bool(true)})
	require.NotNilf(t, findInstanceStatus(unscoped, instanceID),
		"unscoped IncludeAllInstances describe omitted stopped guest %s", instanceID)
	harness.Detail(t, "unscoped_statuses", len(unscoped))

	// --- Stage 7: status-specific filters narrow ---

	harness.Step(t, "describe-instance-status --filter instance-state-name=stopped")
	matched := describeInstanceStatuses(t, fix, &ec2.DescribeInstanceStatusInput{
		InstanceIds:         []*string{aws.String(instanceID)},
		IncludeAllInstances: aws.Bool(true),
		Filters:             []*ec2.Filter{instStatusFilter("instance-state-name", "stopped")},
	})
	require.NotNilf(t, findInstanceStatus(matched, instanceID),
		"instance-state-name=stopped must return the stopped guest %s", instanceID)

	harness.Step(t, "describe-instance-status --filter instance-state-name=running")
	unmatched := describeInstanceStatuses(t, fix, &ec2.DescribeInstanceStatusInput{
		InstanceIds:         []*string{aws.String(instanceID)},
		IncludeAllInstances: aws.Bool(true),
		Filters:             []*ec2.Filter{instStatusFilter("instance-state-name", "running")},
	})
	require.Nilf(t, findInstanceStatus(unmatched, instanceID),
		"instance-state-name=running must not return the stopped guest %s", instanceID)

	// Mulga's health model has one static value per status field, so the AWS
	// status.* filters are deliberately absent from the valid-filter map.
	harness.Step(t, "describe-instance-status --filter instance-status.status (unsupported)")
	harness.ExpectError(t, "InvalidParameterValue", func() error {
		_, derr := fix.AWS.EC2.DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{
			InstanceIds: []*string{aws.String(instanceID)},
			Filters:     []*ec2.Filter{instStatusFilter("instance-status.status", instStatusOK)},
		})
		return derr
	})

	// --- Stage 8: terminated instances never appear ---

	harness.Step(t, "terminate-instances %s", instanceID)
	_, err = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "terminate-instances %s", instanceID)
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "terminated")

	// Termination moves the guest from the stopped bucket to the terminated
	// bucket, which DescribeInstanceStatus never reads from — poll so the
	// assertion bites on the settled state, not on the move being in flight.
	harness.Step(t, "describe-instance-status %s --include-all-instances (terminated)", instanceID)
	harness.EventuallyErr(t, func() error {
		statuses := describeInstanceStatuses(t, fix, &ec2.DescribeInstanceStatusInput{
			InstanceIds:         []*string{aws.String(instanceID)},
			IncludeAllInstances: aws.Bool(true),
		})
		if s := findInstanceStatus(statuses, instanceID); s != nil {
			return fmt.Errorf("terminated guest %s still returned with IncludeAllInstances (state=%s)",
				instanceID, aws.StringValue(s.InstanceState.Name))
		}
		return nil
	}, 60*time.Second, 2*time.Second)
}

// describeInstanceStatuses issues one DescribeInstanceStatus and returns the
// statuses, failing if any InstanceId repeats. Gateway-side dedup is the only
// thing keeping a stop/start race from returning an id twice, and SDK waiters
// assume one entry per id, so every describe in this suite pays for the check.
func describeInstanceStatuses(t *testing.T, fix *Fixture, in *ec2.DescribeInstanceStatusInput) []*ec2.InstanceStatus {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeInstanceStatus(in)
	require.NoError(t, err, "describe-instance-status")
	seen := make(map[string]bool, len(out.InstanceStatuses))
	for _, s := range out.InstanceStatuses {
		id := aws.StringValue(s.InstanceId)
		require.NotEmpty(t, id, "describe-instance-status returned an entry with no InstanceId")
		require.Falsef(t, seen[id], "describe-instance-status returned %s twice", id)
		seen[id] = true
	}
	return out.InstanceStatuses
}

// requireInstanceStatus runs the describe and returns the entry for instanceID,
// failing if it is absent.
func requireInstanceStatus(t *testing.T, fix *Fixture, instanceID string, in *ec2.DescribeInstanceStatusInput) *ec2.InstanceStatus {
	t.Helper()
	statuses := describeInstanceStatuses(t, fix, in)
	s := findInstanceStatus(statuses, instanceID)
	require.NotNilf(t, s, "describe-instance-status returned no entry for %s (%d entries)", instanceID, len(statuses))
	return s
}

// findInstanceStatus returns the entry for instanceID, or nil when absent.
func findInstanceStatus(statuses []*ec2.InstanceStatus, instanceID string) *ec2.InstanceStatus {
	for _, s := range statuses {
		if aws.StringValue(s.InstanceId) == instanceID {
			return s
		}
	}
	return nil
}

// requireReachability asserts the summary carries exactly the reachability
// detail at the wanted status, returning it so callers can assert ImpairedSince.
func requireReachability(t *testing.T, summary *ec2.InstanceStatusSummary, want string) *ec2.InstanceStatusDetails {
	t.Helper()
	require.NotNil(t, summary, "status summary missing")
	require.Lenf(t, summary.Details, 1, "expected exactly 1 status detail, got %d", len(summary.Details))
	detail := summary.Details[0]
	require.Equal(t, instStatusReachability, aws.StringValue(detail.Name), "status detail name")
	require.Equal(t, want, aws.StringValue(detail.Status), "reachability detail status")
	return detail
}

func instStatusFilter(name string, values ...string) *ec2.Filter {
	return &ec2.Filter{Name: aws.String(name), Values: aws.StringSlice(values)}
}
