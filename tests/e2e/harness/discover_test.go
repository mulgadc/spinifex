//go:build e2e

package harness

import (
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// fakeDiscoverEC2 extends fakeEC2 with the three describe surfaces the
// Discover* helpers exercise. Embeds fakeEC2 so the existing key-pair
// fakes stay usable in the same suite if a future test combines flows.
type fakeDiscoverEC2 struct {
	fakeEC2

	azCalls           atomic.Int64
	instanceTypeCalls atomic.Int64
	describeImgCalls  atomic.Int64

	zones      []*ec2.AvailabilityZone
	zonesErr   error
	itypes     []*ec2.InstanceTypeInfo
	itypesErr  error
	imagesByID map[string][]*ec2.Image
	imagesErr  error
}

func (f *fakeDiscoverEC2) DescribeAvailabilityZones(*ec2.DescribeAvailabilityZonesInput) (*ec2.DescribeAvailabilityZonesOutput, error) {
	f.azCalls.Add(1)
	if f.zonesErr != nil {
		return nil, f.zonesErr
	}
	return &ec2.DescribeAvailabilityZonesOutput{AvailabilityZones: f.zones}, nil
}

func (f *fakeDiscoverEC2) DescribeInstanceTypes(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
	f.instanceTypeCalls.Add(1)
	if f.itypesErr != nil {
		return nil, f.itypesErr
	}
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: f.itypes}, nil
}

func (f *fakeDiscoverEC2) DescribeImages(in *ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
	f.describeImgCalls.Add(1)
	if f.imagesErr != nil {
		return nil, f.imagesErr
	}
	// DescribeImages is called twice — once by the name-filter lookup,
	// once by EnsureAMI's state-available poll using ImageIds. The fake
	// returns the same image set for both paths via a single keyed map.
	for _, fv := range in.Filters {
		if aws.StringValue(fv.Name) != "name" {
			continue
		}
		for _, v := range fv.Values {
			if imgs, ok := f.imagesByID[aws.StringValue(v)]; ok {
				return &ec2.DescribeImagesOutput{Images: imgs}, nil
			}
		}
	}
	for _, id := range in.ImageIds {
		for _, imgs := range f.imagesByID {
			for _, img := range imgs {
				if aws.StringValue(img.ImageId) == aws.StringValue(id) {
					return &ec2.DescribeImagesOutput{Images: []*ec2.Image{img}}, nil
				}
			}
		}
	}
	return &ec2.DescribeImagesOutput{}, nil
}

func newDiscoverFixture(t *testing.T) (*Fixture, *fakeDiscoverEC2) {
	t.Helper()
	ec2c := &fakeDiscoverEC2{}
	fx := newFixture(t, ec2c, &fakeELB{})
	return fx, ec2c
}

// TestDiscoverDefaultAZ_FirstAvailable returns the first AZ reported as
// "available" and caches the answer for subsequent calls.
func TestDiscoverDefaultAZ_FirstAvailable(t *testing.T) {
	fx, ec2c := newDiscoverFixture(t)
	ec2c.zones = []*ec2.AvailabilityZone{
		{ZoneName: aws.String("ap-southeast-2a"), State: aws.String("available")},
	}

	got := DiscoverDefaultAZ(t, fx)
	if got != "ap-southeast-2a" {
		t.Fatalf("DiscoverDefaultAZ = %q, want ap-southeast-2a", got)
	}

	// Second call must hit the memo, not the API.
	second := DiscoverDefaultAZ(t, fx)
	if second != got {
		t.Fatalf("second call = %q, want cached %q", second, got)
	}
	if calls := ec2c.azCalls.Load(); calls != 1 {
		t.Fatalf("DescribeAvailabilityZones calls = %d, want 1 (second call should be cached)", calls)
	}
}

// TestDiscoverNanoInstanceType_PicksFirstNano picks the first instance type
// whose name contains "nano" and reports the architecture off its
// ProcessorInfo. Memoized.
func TestDiscoverNanoInstanceType_PicksFirstNano(t *testing.T) {
	fx, ec2c := newDiscoverFixture(t)
	ec2c.itypes = []*ec2.InstanceTypeInfo{
		{
			InstanceType: aws.String("t3.micro"),
			ProcessorInfo: &ec2.ProcessorInfo{
				SupportedArchitectures: []*string{aws.String("x86_64")},
			},
		},
		{
			InstanceType: aws.String("t3.nano"),
			ProcessorInfo: &ec2.ProcessorInfo{
				SupportedArchitectures: []*string{aws.String("x86_64")},
			},
		},
		{
			InstanceType: aws.String("t4g.nano"),
			ProcessorInfo: &ec2.ProcessorInfo{
				SupportedArchitectures: []*string{aws.String("arm64")},
			},
		},
	}

	got, arch := DiscoverNanoInstanceType(t, fx)
	if got != "t3.nano" {
		t.Fatalf("instance type = %q, want t3.nano", got)
	}
	if arch != "x86_64" {
		t.Fatalf("arch = %q, want x86_64", arch)
	}

	// Second call hits the memo.
	got2, arch2 := DiscoverNanoInstanceType(t, fx)
	if got2 != got || arch2 != arch {
		t.Fatalf("second call returned (%q, %q), want cached (%q, %q)", got2, arch2, got, arch)
	}
	if calls := ec2c.instanceTypeCalls.Load(); calls != 1 {
		t.Fatalf("DescribeInstanceTypes calls = %d, want 1", calls)
	}
}

// TestDiscoverUbuntuAMI_Prefers26 picks the v6+ ubuntu-26.04 image when it
// is present alongside the legacy v3 ubuntu-24.04 candidate.
func TestDiscoverUbuntuAMI_Prefers26(t *testing.T) {
	fx, ec2c := newDiscoverFixture(t)
	const arch = "x86_64"
	ec2c.imagesByID = map[string][]*ec2.Image{
		"ami-ubuntu-26.04-" + arch: {{
			ImageId: aws.String("ami-aaaa"),
			State:   aws.String("available"),
		}},
		"ami-ubuntu-24.04-" + arch: {{
			ImageId: aws.String("ami-bbbb"),
			State:   aws.String("available"),
		}},
	}

	got := DiscoverUbuntuAMI(t, fx, arch)
	if got != "ami-aaaa" {
		t.Fatalf("AMI = %q, want ami-aaaa (26.04 preferred)", got)
	}
}

// TestDiscoverUbuntuAMI_FallsBackTo24 returns the legacy 24.04 image when
// the v6+ name is absent (still the pre-gold-image-bump environments).
func TestDiscoverUbuntuAMI_FallsBackTo24(t *testing.T) {
	fx, ec2c := newDiscoverFixture(t)
	const arch = "arm64"
	ec2c.imagesByID = map[string][]*ec2.Image{
		"ami-ubuntu-24.04-" + arch: {{
			ImageId: aws.String("ami-legacy"),
			State:   aws.String("available"),
		}},
	}

	got := DiscoverUbuntuAMI(t, fx, arch)
	if got != "ami-legacy" {
		t.Fatalf("AMI = %q, want ami-legacy", got)
	}
}
