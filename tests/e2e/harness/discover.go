//go:build e2e

// Cluster-discovery helpers that memoize EC2 catalog lookups on the Fixture.
// First call pays the API cost; later callers in the same process hit the cache.
package harness

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// DiscoverDefaultAZ returns the first AZ reported as "available" by
// DescribeAvailabilityZones. Memoized per fixture.
func DiscoverDefaultAZ(t *testing.T, fx *Fixture) string {
	t.Helper()
	az, err := fx.ensureOnce(t, "discover:default-az", func() (string, func() error, error) {
		out, derr := fx.EC2.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{})
		if derr != nil {
			return "", nil, fmt.Errorf("DescribeAvailabilityZones: %w", derr)
		}
		if len(out.AvailabilityZones) == 0 {
			return "", nil, fmt.Errorf("no availability zones returned")
		}
		name := aws.StringValue(out.AvailabilityZones[0].ZoneName)
		state := aws.StringValue(out.AvailabilityZones[0].State)
		if state != "available" {
			return "", nil, fmt.Errorf("AZ %s state %q (want available)", name, state)
		}
		return name, nil, nil
	})
	if err != nil {
		t.Fatalf("DiscoverDefaultAZ: %v", err)
	}
	return az
}

// DiscoverNanoInstanceType returns the first instance type whose name contains
// "nano" along with the architecture reported by its ProcessorInfo. Memoized
// per fixture. Mirrors the picker logic from the bash run-e2e.sh driver.
func DiscoverNanoInstanceType(t *testing.T, fx *Fixture) (instanceType, arch string) {
	t.Helper()
	combined, err := fx.ensureOnce(t, "discover:nano-instance-type", func() (string, func() error, error) {
		out, derr := fx.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
		if derr != nil {
			return "", nil, fmt.Errorf("DescribeInstanceTypes: %w", derr)
		}
		for _, it := range out.InstanceTypes {
			name := aws.StringValue(it.InstanceType)
			if !strings.Contains(name, "nano") {
				continue
			}
			if it.ProcessorInfo == nil || len(it.ProcessorInfo.SupportedArchitectures) == 0 {
				continue
			}
			a := aws.StringValue(it.ProcessorInfo.SupportedArchitectures[0])
			// Pack into a single memo value (string) — split below.
			return name + "|" + a, nil, nil
		}
		return "", nil, fmt.Errorf("no nano instance type available (saw %d types)", len(out.InstanceTypes))
	})
	if err != nil {
		t.Fatalf("DiscoverNanoInstanceType: %v", err)
	}
	parts := strings.SplitN(combined, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("DiscoverNanoInstanceType: malformed memo value %q", combined)
	}
	return parts[0], parts[1]
}

// DiscoverInstanceTypeAtLeastMemory returns the smallest instance type whose
// advertised memory is >= minMemMiB, along with its architecture. Memoized
// per (minMemMiB) on the Fixture like the other Discover* helpers.
//
// DiscoverNanoInstanceType always returns the smallest catalog entry
// (~512MiB), which is the right choice when a test wants "as little guest as
// will boot" but the wrong one when a test needs a *specific* memory
// ceiling: sizing a multi-hundred-MiB workload against nano OOM-thrashes the
// guest instead of exercising the condition under test. This picks the
// smallest type that still clears the requested floor, so a caller gets a
// deterministic, minimal-headroom guest instead of whatever DescribeInstanceTypes
// happens to return first for a >= comparison done by hand.
func DiscoverInstanceTypeAtLeastMemory(t *testing.T, fx *Fixture, minMemMiB int64) (instanceType, arch string) {
	t.Helper()
	if minMemMiB <= 0 {
		t.Fatalf("DiscoverInstanceTypeAtLeastMemory: minMemMiB must be positive, got %d", minMemMiB)
	}
	key := fmt.Sprintf("discover:min-memory-instance-type:%d", minMemMiB)
	combined, err := fx.ensureOnce(t, key, func() (string, func() error, error) {
		out, derr := fx.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
		if derr != nil {
			return "", nil, fmt.Errorf("DescribeInstanceTypes: %w", derr)
		}
		var bestName, bestArch string
		bestMem := int64(-1)
		for _, it := range out.InstanceTypes {
			if it.MemoryInfo == nil || it.ProcessorInfo == nil || len(it.ProcessorInfo.SupportedArchitectures) == 0 {
				continue
			}
			mem := aws.Int64Value(it.MemoryInfo.SizeInMiB)
			if mem < minMemMiB {
				continue
			}
			// Keep the tightest fit seen so far — the smallest type that still
			// clears the floor, not merely the first one DescribeInstanceTypes
			// happens to enumerate.
			if bestMem == -1 || mem < bestMem {
				bestMem = mem
				bestName = aws.StringValue(it.InstanceType)
				bestArch = aws.StringValue(it.ProcessorInfo.SupportedArchitectures[0])
			}
		}
		if bestName == "" {
			return "", nil, fmt.Errorf("no instance type with memory >= %d MiB available (saw %d types)", minMemMiB, len(out.InstanceTypes))
		}
		return bestName + "|" + bestArch, nil, nil
	})
	if err != nil {
		t.Fatalf("DiscoverInstanceTypeAtLeastMemory: %v", err)
	}
	parts := strings.SplitN(combined, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("DiscoverInstanceTypeAtLeastMemory: malformed memo value %q", combined)
	}
	return parts[0], parts[1]
}

// DiscoverUbuntuAMI returns the AMI ID for the architecture-appropriate Ubuntu
// gold image. Tries ubuntu-26.04 first, falls back to ubuntu-24.04.
// Routes through EnsureAMI so the state-available poll and memoization apply.
func DiscoverUbuntuAMI(t *testing.T, fx *Fixture, arch string) string {
	t.Helper()
	if arch == "" {
		t.Fatalf("DiscoverUbuntuAMI: arch required")
	}
	id, err := fx.ensureOnce(t, "discover:ubuntu-ami:"+arch, func() (string, func() error, error) {
		candidates := []string{
			"ami-ubuntu-26.04-" + arch,
			"ami-ubuntu-24.04-" + arch,
		}
		for _, name := range candidates {
			out, derr := fx.EC2.DescribeImages(&ec2.DescribeImagesInput{
				Filters: []*ec2.Filter{
					{Name: aws.String("name"), Values: []*string{aws.String(name)}},
				},
			})
			if derr == nil && len(out.Images) > 0 {
				return aws.StringValue(out.Images[0].ImageId), nil, nil
			}
		}
		return "", nil, fmt.Errorf("no Ubuntu AMI found (tried: %v)", candidates)
	})
	if err != nil {
		t.Fatalf("DiscoverUbuntuAMI: %v", err)
	}
	return EnsureAMI(t, fx, AMISource{Existing: id})
}
