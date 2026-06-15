//go:build e2e

package harness

import (
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
)

// sweepFakeEC2 returns a seeded set of resources from each Describe* and
// records every delete/terminate call. The embedded ec2iface.EC2API panics on
// any un-mocked method so the sweep can't silently grow a new dependency.
type sweepFakeEC2 struct {
	ec2iface.EC2API

	instances      []*ec2.Instance
	natGateways    []*ec2.NatGateway
	snapshots      []*ec2.Snapshot
	volumes        []*ec2.Volume
	images         []*ec2.Image
	securityGroups []*ec2.SecurityGroup
	subnets        []*ec2.Subnet
	keyPairs       []*ec2.KeyPairInfo

	deleted   map[string][]string
	tagged    map[string][]string // runID → resource IDs passed to CreateTags
	describeN map[string]int
}

func newSweepFakeEC2() *sweepFakeEC2 {
	return &sweepFakeEC2{deleted: map[string][]string{}, tagged: map[string][]string{}, describeN: map[string]int{}}
}

func (f *sweepFakeEC2) rec(kind string, ids ...string) {
	f.deleted[kind] = append(f.deleted[kind], ids...)
}

func (f *sweepFakeEC2) CreateTags(in *ec2.CreateTagsInput) (*ec2.CreateTagsOutput, error) {
	var runID string
	for _, t := range in.Tags {
		if aws.StringValue(t.Key) == runTagKey {
			runID = aws.StringValue(t.Value)
		}
	}
	f.tagged[runID] = append(f.tagged[runID], aws.StringValueSlice(in.Resources)...)
	return &ec2.CreateTagsOutput{}, nil
}

func (f *sweepFakeEC2) DescribeInstances(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
	// Wait-for-terminated path describes by explicit IDs — report them gone so
	// waitInstancesTerminated returns on the first poll.
	if len(in.InstanceIds) > 0 {
		var insts []*ec2.Instance
		for _, id := range in.InstanceIds {
			insts = append(insts, &ec2.Instance{
				InstanceId: id,
				State:      &ec2.InstanceState{Name: aws.String("terminated")},
			})
		}
		return &ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{{Instances: insts}},
		}, nil
	}
	f.describeN["instances"]++
	return &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{Instances: f.instances}},
	}, nil
}

func (f *sweepFakeEC2) TerminateInstances(in *ec2.TerminateInstancesInput) (*ec2.TerminateInstancesOutput, error) {
	f.rec("instance", aws.StringValueSlice(in.InstanceIds)...)
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *sweepFakeEC2) DescribeNatGateways(*ec2.DescribeNatGatewaysInput) (*ec2.DescribeNatGatewaysOutput, error) {
	f.describeN["natgateways"]++
	return &ec2.DescribeNatGatewaysOutput{NatGateways: f.natGateways}, nil
}

func (f *sweepFakeEC2) DeleteNatGateway(in *ec2.DeleteNatGatewayInput) (*ec2.DeleteNatGatewayOutput, error) {
	f.rec("natgateway", aws.StringValue(in.NatGatewayId))
	return &ec2.DeleteNatGatewayOutput{}, nil
}

func (f *sweepFakeEC2) DescribeSnapshots(*ec2.DescribeSnapshotsInput) (*ec2.DescribeSnapshotsOutput, error) {
	f.describeN["snapshots"]++
	return &ec2.DescribeSnapshotsOutput{Snapshots: f.snapshots}, nil
}

func (f *sweepFakeEC2) DeleteSnapshot(in *ec2.DeleteSnapshotInput) (*ec2.DeleteSnapshotOutput, error) {
	f.rec("snapshot", aws.StringValue(in.SnapshotId))
	return &ec2.DeleteSnapshotOutput{}, nil
}

func (f *sweepFakeEC2) DescribeVolumes(*ec2.DescribeVolumesInput) (*ec2.DescribeVolumesOutput, error) {
	f.describeN["volumes"]++
	return &ec2.DescribeVolumesOutput{Volumes: f.volumes}, nil
}

func (f *sweepFakeEC2) DeleteVolume(in *ec2.DeleteVolumeInput) (*ec2.DeleteVolumeOutput, error) {
	f.rec("volume", aws.StringValue(in.VolumeId))
	return &ec2.DeleteVolumeOutput{}, nil
}

func (f *sweepFakeEC2) DescribeImages(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
	f.describeN["images"]++
	return &ec2.DescribeImagesOutput{Images: f.images}, nil
}

func (f *sweepFakeEC2) DeregisterImage(in *ec2.DeregisterImageInput) (*ec2.DeregisterImageOutput, error) {
	f.rec("image", aws.StringValue(in.ImageId))
	return &ec2.DeregisterImageOutput{}, nil
}

func (f *sweepFakeEC2) DescribeSecurityGroups(*ec2.DescribeSecurityGroupsInput) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.describeN["securitygroups"]++
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: f.securityGroups}, nil
}

func (f *sweepFakeEC2) DeleteSecurityGroup(in *ec2.DeleteSecurityGroupInput) (*ec2.DeleteSecurityGroupOutput, error) {
	f.rec("securitygroup", aws.StringValue(in.GroupId))
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (f *sweepFakeEC2) DescribeSubnets(*ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error) {
	f.describeN["subnets"]++
	return &ec2.DescribeSubnetsOutput{Subnets: f.subnets}, nil
}

func (f *sweepFakeEC2) DeleteSubnet(in *ec2.DeleteSubnetInput) (*ec2.DeleteSubnetOutput, error) {
	f.rec("subnet", aws.StringValue(in.SubnetId))
	return &ec2.DeleteSubnetOutput{}, nil
}

func (f *sweepFakeEC2) DescribeKeyPairs(*ec2.DescribeKeyPairsInput) (*ec2.DescribeKeyPairsOutput, error) {
	f.describeN["keypairs"]++
	return &ec2.DescribeKeyPairsOutput{KeyPairs: f.keyPairs}, nil
}

func (f *sweepFakeEC2) DeleteKeyPair(in *ec2.DeleteKeyPairInput) (*ec2.DeleteKeyPairOutput, error) {
	f.rec("keypair", aws.StringValue(in.KeyName))
	return &ec2.DeleteKeyPairOutput{}, nil
}

// sweepFakeELB seeds DescribeLoadBalancers + per-ARN DescribeTags and records
// deletes / AddTags.
type sweepFakeELB struct {
	elbv2iface.ELBV2API

	lbs     []*elbv2.LoadBalancer
	tags    map[string][]*elbv2.Tag // ARN → tags
	deleted []string
	added   map[string][]string // runID → ARNs passed to AddTags
}

func newSweepFakeELB() *sweepFakeELB {
	return &sweepFakeELB{tags: map[string][]*elbv2.Tag{}, added: map[string][]string{}}
}

func (f *sweepFakeELB) DescribeLoadBalancers(*elbv2.DescribeLoadBalancersInput) (*elbv2.DescribeLoadBalancersOutput, error) {
	return &elbv2.DescribeLoadBalancersOutput{LoadBalancers: f.lbs}, nil
}

func (f *sweepFakeELB) DescribeTags(in *elbv2.DescribeTagsInput) (*elbv2.DescribeTagsOutput, error) {
	var descs []*elbv2.TagDescription
	for _, arn := range in.ResourceArns {
		descs = append(descs, &elbv2.TagDescription{
			ResourceArn: arn,
			Tags:        f.tags[aws.StringValue(arn)],
		})
	}
	return &elbv2.DescribeTagsOutput{TagDescriptions: descs}, nil
}

func (f *sweepFakeELB) DeleteLoadBalancer(in *elbv2.DeleteLoadBalancerInput) (*elbv2.DeleteLoadBalancerOutput, error) {
	f.deleted = append(f.deleted, aws.StringValue(in.LoadBalancerArn))
	return &elbv2.DeleteLoadBalancerOutput{}, nil
}

func (f *sweepFakeELB) AddTags(in *elbv2.AddTagsInput) (*elbv2.AddTagsOutput, error) {
	var runID string
	for _, t := range in.Tags {
		if aws.StringValue(t.Key) == runTagKey {
			runID = aws.StringValue(t.Value)
		}
	}
	f.added[runID] = append(f.added[runID], aws.StringValueSlice(in.ResourceArns)...)
	return &elbv2.AddTagsOutput{}, nil
}

var (
	_ ec2iface.EC2API     = (*sweepFakeEC2)(nil)
	_ elbv2iface.ELBV2API = (*sweepFakeELB)(nil)
)

// TestSweepRunResources_BlankRunIDNoop verifies a blank run id deletes nothing
// and never dials AWS.
func TestSweepRunResources_BlankRunIDNoop(t *testing.T) {
	ec2c := newSweepFakeEC2()
	elbc := newSweepFakeELB()

	rep := SweepRunResources(ec2c, elbc, "")

	if len(rep.Deleted) != 0 || len(rep.Errors) != 0 {
		t.Fatalf("blank run id: report = %+v, want empty", rep)
	}
	if len(ec2c.describeN) != 0 {
		t.Fatalf("blank run id issued Describe calls: %v", ec2c.describeN)
	}
}

// TestSweepRunResources_DeletesEachKind seeds one live resource of every kind
// and verifies the sweep deletes them all and records them in the report.
func TestSweepRunResources_DeletesEachKind(t *testing.T) {
	ec2c := newSweepFakeEC2()
	ec2c.instances = []*ec2.Instance{{
		InstanceId: aws.String("i-1"),
		State:      &ec2.InstanceState{Name: aws.String("running")},
	}}
	ec2c.natGateways = []*ec2.NatGateway{{
		NatGatewayId: aws.String("nat-1"), State: aws.String("available"),
	}}
	ec2c.snapshots = []*ec2.Snapshot{{SnapshotId: aws.String("snap-1")}}
	ec2c.volumes = []*ec2.Volume{{VolumeId: aws.String("vol-1"), State: aws.String("available")}}
	ec2c.images = []*ec2.Image{{ImageId: aws.String("ami-1")}}
	ec2c.securityGroups = []*ec2.SecurityGroup{{GroupId: aws.String("sg-1"), GroupName: aws.String("e2e-sg")}}
	ec2c.subnets = []*ec2.Subnet{{SubnetId: aws.String("subnet-1")}}
	ec2c.keyPairs = []*ec2.KeyPairInfo{{KeyName: aws.String("e2e-key")}}

	arn := "arn:lb/1"
	elbc := newSweepFakeELB()
	elbc.lbs = []*elbv2.LoadBalancer{{LoadBalancerArn: aws.String(arn)}}
	elbc.tags[arn] = []*elbv2.Tag{{Key: aws.String(runTagKey), Value: aws.String("run-1")}}

	rep := SweepRunResources(ec2c, elbc, "run-1")

	want := map[string]string{
		"instance": "i-1", "natgateway": "nat-1", "snapshot": "snap-1",
		"volume": "vol-1", "image": "ami-1", "securitygroup": "sg-1",
		"subnet": "subnet-1", "keypair": "e2e-key", "loadbalancer": arn,
	}
	for kind, id := range want {
		got := rep.Deleted[kind]
		if len(got) != 1 || got[0] != id {
			t.Errorf("Deleted[%s] = %v, want [%s]", kind, got, id)
		}
	}
	if len(rep.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", rep.Errors)
	}
}

// TestSweepInstances_SkipsTerminated verifies already-terminated instances are
// not re-terminated.
func TestSweepInstances_SkipsTerminated(t *testing.T) {
	ec2c := newSweepFakeEC2()
	ec2c.instances = []*ec2.Instance{{
		InstanceId: aws.String("i-dead"),
		State:      &ec2.InstanceState{Name: aws.String("terminated")},
	}}
	rep := SweepRunResources(ec2c, newSweepFakeELB(), "run-1")
	if len(rep.Deleted["instance"]) != 0 {
		t.Fatalf("terminated instance swept: %v", rep.Deleted["instance"])
	}
}

// TestSweepSecurityGroups_SkipsDefault verifies the default SG is never
// deleted even if it carries the run tag.
func TestSweepSecurityGroups_SkipsDefault(t *testing.T) {
	ec2c := newSweepFakeEC2()
	ec2c.securityGroups = []*ec2.SecurityGroup{
		{GroupId: aws.String("sg-def"), GroupName: aws.String("default")},
		{GroupId: aws.String("sg-e2e"), GroupName: aws.String("e2e")},
	}
	rep := SweepRunResources(ec2c, newSweepFakeELB(), "run-1")
	got := rep.Deleted["securitygroup"]
	if len(got) != 1 || got[0] != "sg-e2e" {
		t.Fatalf("securitygroup deleted = %v, want [sg-e2e]", got)
	}
}

// TestSweepLoadBalancers_MatchesByTag verifies only LBs carrying the matching
// run tag are deleted (ELBv2 Describe has no tag filter).
func TestSweepLoadBalancers_MatchesByTag(t *testing.T) {
	elbc := newSweepFakeELB()
	elbc.lbs = []*elbv2.LoadBalancer{
		{LoadBalancerArn: aws.String("arn:match")},
		{LoadBalancerArn: aws.String("arn:other")},
		{LoadBalancerArn: aws.String("arn:untagged")},
	}
	elbc.tags["arn:match"] = []*elbv2.Tag{{Key: aws.String(runTagKey), Value: aws.String("run-1")}}
	elbc.tags["arn:other"] = []*elbv2.Tag{{Key: aws.String(runTagKey), Value: aws.String("run-2")}}

	SweepRunResources(newSweepFakeEC2(), elbc, "run-1")

	sort.Strings(elbc.deleted)
	if len(elbc.deleted) != 1 || elbc.deleted[0] != "arn:match" {
		t.Fatalf("deleted LBs = %v, want [arn:match]", elbc.deleted)
	}
}

// TestSweepLoadBalancers_NilClientNoop verifies a nil ELBv2 client is tolerated
// (suites with no LB surface).
func TestSweepLoadBalancers_NilClientNoop(t *testing.T) {
	rep := SweepRunResources(newSweepFakeEC2(), nil, "run-1")
	if len(rep.Deleted["loadbalancer"]) != 0 {
		t.Fatalf("nil elbc swept LBs: %v", rep.Deleted["loadbalancer"])
	}
}

// TestTagRunResources_NoRunIDSkips verifies tagging no-ops without a run id.
func TestTagRunResources_NoRunIDSkips(t *testing.T) {
	ec2c := newSweepFakeEC2()
	fx := newFixture(t, ec2c, newSweepFakeELB())
	fx.runID = ""

	fx.tagRunResources("i-1")
	if len(ec2c.tagged) != 0 {
		t.Fatalf("tagRunResources tagged with empty run id: %v", ec2c.tagged)
	}
}

// TestTagRunResources_StampsRunTag verifies the configured run id is stamped.
func TestTagRunResources_StampsRunTag(t *testing.T) {
	ec2c := newSweepFakeEC2()
	fx := newFixture(t, ec2c, newSweepFakeELB())
	fx.runID = "run-9"

	fx.tagRunResources("vol-1", "vol-2")
	got := ec2c.tagged["run-9"]
	if len(got) != 2 || got[0] != "vol-1" || got[1] != "vol-2" {
		t.Fatalf("tagged[run-9] = %v, want [vol-1 vol-2]", got)
	}
}

// TestTagRunELB_StampsRunTag verifies the ELBv2 AddTags path stamps the run id.
func TestTagRunELB_StampsRunTag(t *testing.T) {
	elbc := newSweepFakeELB()
	fx := newFixture(t, newSweepFakeEC2(), elbc)
	fx.runID = "run-9"

	fx.tagRunELB("arn:lb/1")
	got := elbc.added["run-9"]
	if len(got) != 1 || got[0] != "arn:lb/1" {
		t.Fatalf("added[run-9] = %v, want [arn:lb/1]", got)
	}
}
