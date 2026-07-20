//go:build e2e

package multinode

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// needAZ returns the discovered default AZ, memoized on the harness fixture.
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

// needKeyPair ensures a test-scoped EC2 key pair and returns its name and PEM path.
// Memoized on the harness fixture so all tests share one key.
func needKeyPair(t *testing.T, fix *Fixture) (name, pemPath string) {
	t.Helper()
	return harness.EnsureKeyPair(t, fix.Harness, fix.Artifacts)
}

// Package-scoped trio. Whichever Test* runs first triggers the launch; every other caller of needInstanceTrio reuses the same IDs.
var (
	trioOnce sync.Once
	trioIDs  []string
	trioErr  error
)

// needInstanceTrio returns the package singleton trio of nano instances on the default
// VPC, launching them once and registering process-exit termination cleanup.
func needInstanceTrio(t *testing.T, fix *Fixture) []string {
	t.Helper()
	trioOnce.Do(func() {
		instType, arch := needInstanceTypeArch(t, fix)
		amiID := needAMI(t, fix, arch)
		keyName, _ := needKeyPair(t, fix)
		def := harness.EnsureDefaultVPC(t, fix.Harness)
		require.NotEmpty(t, def.SGID, "default SG required")
		harness.AuthorizeSSHIngress(t, fix.AWS, def.SGID)

		// Each launch is retried on InsufficientInstanceCapacity: the resourceMgr
		// can briefly report 0 capacity right after daemon start before its first
		// inventory scan completes. Treat as transiently retryable.
		input := &ec2.RunInstancesInput{
			ImageId:          aws.String(amiID),
			InstanceType:     aws.String(instType),
			KeyName:          aws.String(keyName),
			SubnetId:         aws.String(def.SubnetID),
			SecurityGroupIds: []*string{aws.String(def.SGID)},
			MinCount:         aws.Int64(1),
			MaxCount:         aws.Int64(1),
		}
		for i := 0; i < 3; i++ {
			var out *ec2.Reservation
			var err error
			for attempt := 1; attempt <= 6; attempt++ {
				out, err = fix.AWS.EC2.RunInstances(input)
				if err == nil {
					break
				}
				if !strings.Contains(err.Error(), "InsufficientInstanceCapacity") {
					break
				}
				t.Logf("needInstanceTrio: launch %d attempt %d: InsufficientInstanceCapacity, retrying in 10s", i, attempt)
				time.Sleep(10 * time.Second)
			}
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
			// Stagger encourages distribution across nodes (bash sleep 2).
			if i < 2 {
				time.Sleep(2 * time.Second)
			}
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

// readyNodeCount counts "Ready" lines in spx get nodes output (substring, case-sensitive).
func readyNodeCount(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Ready") {
			n++
		}
	}
	return n
}
