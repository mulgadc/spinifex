//go:build e2e

package multinode

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// needAZ is a package-local shorthand for the discovered default AZ.
// Memoized on the harness fixture so every Test* gets the same answer
// regardless of execution order.
func needAZ(t *testing.T, fix *Fixture) string {
	t.Helper()
	return harness.DiscoverDefaultAZ(t, fix.Harness)
}

// needInstanceTypeArch returns the discovered nano instance type and its
// architecture. Memoized on the harness fixture.
func needInstanceTypeArch(t *testing.T, fix *Fixture) (instanceType, arch string) {
	t.Helper()
	return harness.DiscoverNanoInstanceType(t, fix.Harness)
}

// needAMI returns the discovered Ubuntu AMI for the given architecture.
// Memoized on the harness fixture.
func needAMI(t *testing.T, fix *Fixture, arch string) string {
	t.Helper()
	return harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
}

// needKeyPair ensures a test-scoped EC2 key pair and returns its name plus
// the on-disk PEM path. Memoized on the harness fixture, so every Test*
// across the package shares the same key (PEM file written once).
func needKeyPair(t *testing.T, fix *Fixture) (name, pemPath string) {
	t.Helper()
	return harness.EnsureKeyPair(t, fix.Harness, fix.Artifacts)
}

// Package-scoped trio. Phase 3 launches; phases 4–7 reuse the same IDs.
// Mirrors the iam_helpers_test sync.Once pattern from tests/e2e/single.
var (
	trioOnce sync.Once
	trioIDs  []string
	trioErr  error
)

// needInstanceTrio returns the package singleton trio of nano instances on
// the default VPC, launching them once on first call and registering
// terminate-on-process-exit. Mirrors bash phase 3 which launches 3 stagger.
func needInstanceTrio(t *testing.T, fix *Fixture) []string {
	t.Helper()
	trioOnce.Do(func() {
		instType, arch := needInstanceTypeArch(t, fix)
		amiID := needAMI(t, fix, arch)
		keyName, _ := needKeyPair(t, fix)
		def := harness.EnsureDefaultVPC(t, fix.Harness)
		require.NotEmpty(t, def.SGID, "default SG required")
		harness.AuthorizeSSHIngress(t, fix.AWS, def.SGID)

		for i := 0; i < 3; i++ {
			out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
				ImageId:          aws.String(amiID),
				InstanceType:     aws.String(instType),
				KeyName:          aws.String(keyName),
				SubnetId:         aws.String(def.SubnetID),
				SecurityGroupIds: []*string{aws.String(def.SGID)},
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
			})
			if err != nil {
				trioErr = err
				return
			}
			if len(out.Instances) == 0 {
				trioErr = fmt.Errorf("RunInstances %d: 0 instances returned", i)
				return
			}
			id := aws.StringValue(out.Instances[0].InstanceId)
			trioIDs = append(trioIDs, id)

			// Process-scoped cleanup — every Test* in the package reuses
			// the trio, so per-test t.Cleanup would tear down too early.
			idCopy := id
			fix.Harness.RegisterCleanup(func() {
				_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
					InstanceIds: []*string{aws.String(idCopy)},
				})
			})
		}

		for _, id := range trioIDs {
			harness.WaitForInstanceState(t, fix.AWS, id, "running")
		}
	})
	if trioErr != nil {
		t.Fatalf("needInstanceTrio: %v", trioErr)
	}
	return trioIDs
}

// readyNodeCount counts the number of "Ready" lines in `spx get nodes`
// output. Bash phase 2 used `grep -c "Ready"`; we match the same
// (substring, case-sensitive) so cluster-status string drift surfaces in
// both tracks identically.
func readyNodeCount(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Ready") {
			n++
		}
	}
	return n
}
