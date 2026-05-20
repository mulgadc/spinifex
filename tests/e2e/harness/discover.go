//go:build e2e

// Cluster-discovery helpers that memoize their answer on the Fixture.
//
// These wrap the read-only EC2 calls the single-node e2e suite uses to learn
// the per-cluster catalog (AZ, nano instance type, Ubuntu AMI) so any top-level
// Test* can resolve them without depending on an earlier phase having stashed
// the result on a shared Fixture field. Memo keys are stable per fixture, so
// the first call pays the API cost and later callers hit the cache.
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

// DiscoverUbuntuAMI returns the AMI ID for the architecture-appropriate Ubuntu
// gold image bootstrap-install.sh staged via `spx admin images import --file`.
// Tries the v6+ ubuntu-26.04 name first, falls back to the v3 ubuntu-24.04
// candidate. Routes the discovered ID through EnsureAMI so the state-available
// poll + memoization match every other AMI consumer in the fixture.
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
