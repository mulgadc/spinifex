//go:build e2e

// Run-scoped resource tagging + teardown sweep (mulga-siv-277).
//
// Every resource the Ensure* fixtures create is tagged e2e:run=<run-id> when
// SPINIFEX_E2E_RUN_ID is set (CI: github.run_id; local: a temp id). A thin
// teardown.test binary then sweeps everything carrying that tag at the end of a
// run — reclaiming resources leaked by a crashed suite, an interrupted run, or
// a persistent (non-tofu-destroyed) cluster. The run-id scoping keeps the sweep
// precise: with no run id, tagging and sweeping both no-op, so the sweep can
// never delete an untagged or foreign resource.
package harness

import (
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
)

const (
	// RunIDEnv carries the cross-process run id shared by every suite binary
	// and the teardown sweep within one e2e run.
	RunIDEnv = "SPINIFEX_E2E_RUN_ID"
	// runTagKey is the EC2/ELBv2 tag key stamped on fixture-created resources.
	runTagKey = "e2e:run"
)

// tagRunResources stamps the run tag on the given EC2 resource IDs when a run
// id is configured. Best-effort: a tagging failure is logged, not fatal — the
// resource still functions, it just won't be reclaimable by the id sweep.
func (f *Fixture) tagRunResources(ids ...string) {
	if f.runID == "" || len(ids) == 0 {
		return
	}
	_, err := f.EC2.CreateTags(&ec2.CreateTagsInput{
		Resources: aws.StringSlice(ids),
		Tags:      []*ec2.Tag{{Key: aws.String(runTagKey), Value: aws.String(f.runID)}},
	})
	if err != nil {
		f.logf("tagRunResources %v: %v", ids, err)
	}
}

// tagRunELB stamps the run tag on an ELBv2 resource (load balancers use a
// distinct AddTags API, not EC2 CreateTags). Best-effort like tagRunResources.
func (f *Fixture) tagRunELB(arns ...string) {
	if f.runID == "" || len(arns) == 0 {
		return
	}
	_, err := f.ELBv2.AddTags(&elbv2.AddTagsInput{
		ResourceArns: aws.StringSlice(arns),
		Tags:         []*elbv2.Tag{{Key: aws.String(runTagKey), Value: aws.String(f.runID)}},
	})
	if err != nil {
		f.logf("tagRunELB %v: %v", arns, err)
	}
}

// SweepReport records what a sweep deleted and any non-fatal errors hit along
// the way. Sweeping is best-effort: one resource's deletion failure never stops
// the rest.
type SweepReport struct {
	Deleted map[string][]string // resource kind → deleted IDs
	Errors  []error
}

func newSweepReport() *SweepReport { return &SweepReport{Deleted: map[string][]string{}} }

func (r *SweepReport) deleted(kind string, ids ...string) {
	if len(ids) > 0 {
		r.Deleted[kind] = append(r.Deleted[kind], ids...)
	}
}

func (r *SweepReport) errf(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Errorf(format, args...))
}

// SweepRunResources deletes every EC2/ELBv2 resource tagged e2e:run=<runID>,
// in dependency order (load balancers → instances → NAT gateways → snapshots →
// volumes → images → security groups → subnets → key pairs). It is idempotent
// and best-effort; callers inspect SweepReport but a non-empty Errors list is
// not necessarily a run failure (resources may already be gone, or still
// settling). A blank runID returns an empty report without touching anything.
func SweepRunResources(ec2c ec2iface.EC2API, elbc elbv2iface.ELBV2API, runID string) *SweepReport {
	rep := newSweepReport()
	if runID == "" {
		return rep
	}

	sweepLoadBalancers(elbc, runID, rep)
	sweepInstances(ec2c, runID, rep)
	sweepNatGateways(ec2c, runID, rep)
	sweepSnapshots(ec2c, runID, rep)
	sweepVolumes(ec2c, runID, rep)
	sweepImages(ec2c, runID, rep)
	sweepSecurityGroups(ec2c, runID, rep)
	sweepSubnets(ec2c, runID, rep)
	sweepKeyPairs(ec2c, runID, rep)

	return rep
}

func runFilter(runID string) []*ec2.Filter {
	return []*ec2.Filter{{
		Name:   aws.String("tag:" + runTagKey),
		Values: []*string{aws.String(runID)},
	}}
}

func sweepInstances(ec2c ec2iface.EC2API, runID string, rep *SweepReport) {
	out, err := ec2c.DescribeInstances(&ec2.DescribeInstancesInput{Filters: runFilter(runID)})
	if err != nil {
		rep.errf("DescribeInstances: %w", err)
		return
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if s := aws.StringValue(inst.State.Name); s == "terminated" || s == "shutting-down" {
				continue
			}
			ids = append(ids, aws.StringValue(inst.InstanceId))
		}
	}
	if len(ids) == 0 {
		return
	}
	if _, err := ec2c.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice(ids),
	}); err != nil {
		rep.errf("TerminateInstances %v: %w", ids, err)
		return
	}
	rep.deleted("instance", ids...)
	// Wait (bounded) for termination so dependent SG/subnet deletes don't fail
	// with DependencyViolation.
	waitInstancesTerminated(ec2c, ids, rep)
}

func waitInstancesTerminated(ec2c ec2iface.EC2API, ids []string, rep *SweepReport) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := ec2c.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: aws.StringSlice(ids),
		})
		if err != nil {
			rep.errf("wait terminate DescribeInstances: %w", err)
			return
		}
		allDone := true
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if aws.StringValue(inst.State.Name) != "terminated" {
					allDone = false
				}
			}
		}
		if allDone || time.Now().After(deadline) {
			return
		}
		time.Sleep(3 * time.Second)
	}
}

func sweepNatGateways(ec2c ec2iface.EC2API, runID string, rep *SweepReport) {
	out, err := ec2c.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{Filter: runFilter(runID)})
	if err != nil {
		rep.errf("DescribeNatGateways: %w", err)
		return
	}
	for _, ngw := range out.NatGateways {
		if s := aws.StringValue(ngw.State); s == "deleted" || s == "deleting" {
			continue
		}
		id := aws.StringValue(ngw.NatGatewayId)
		if _, err := ec2c.DeleteNatGateway(&ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(id)}); err != nil {
			rep.errf("DeleteNatGateway %s: %w", id, err)
			continue
		}
		rep.deleted("natgateway", id)
	}
}

func sweepSnapshots(ec2c ec2iface.EC2API, runID string, rep *SweepReport) {
	out, err := ec2c.DescribeSnapshots(&ec2.DescribeSnapshotsInput{Filters: runFilter(runID)})
	if err != nil {
		rep.errf("DescribeSnapshots: %w", err)
		return
	}
	for _, s := range out.Snapshots {
		id := aws.StringValue(s.SnapshotId)
		if _, err := ec2c.DeleteSnapshot(&ec2.DeleteSnapshotInput{SnapshotId: aws.String(id)}); err != nil {
			rep.errf("DeleteSnapshot %s: %w", id, err)
			continue
		}
		rep.deleted("snapshot", id)
	}
}

func sweepVolumes(ec2c ec2iface.EC2API, runID string, rep *SweepReport) {
	out, err := ec2c.DescribeVolumes(&ec2.DescribeVolumesInput{Filters: runFilter(runID)})
	if err != nil {
		rep.errf("DescribeVolumes: %w", err)
		return
	}
	for _, v := range out.Volumes {
		if aws.StringValue(v.State) == "deleting" {
			continue
		}
		id := aws.StringValue(v.VolumeId)
		if _, err := ec2c.DeleteVolume(&ec2.DeleteVolumeInput{VolumeId: aws.String(id)}); err != nil {
			rep.errf("DeleteVolume %s: %w", id, err)
			continue
		}
		rep.deleted("volume", id)
	}
}

func sweepImages(ec2c ec2iface.EC2API, runID string, rep *SweepReport) {
	out, err := ec2c.DescribeImages(&ec2.DescribeImagesInput{Filters: runFilter(runID)})
	if err != nil {
		rep.errf("DescribeImages: %w", err)
		return
	}
	for _, img := range out.Images {
		id := aws.StringValue(img.ImageId)
		if _, err := ec2c.DeregisterImage(&ec2.DeregisterImageInput{ImageId: aws.String(id)}); err != nil {
			rep.errf("DeregisterImage %s: %w", id, err)
			continue
		}
		rep.deleted("image", id)
	}
}

func sweepSecurityGroups(ec2c ec2iface.EC2API, runID string, rep *SweepReport) {
	out, err := ec2c.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{Filters: runFilter(runID)})
	if err != nil {
		rep.errf("DescribeSecurityGroups: %w", err)
		return
	}
	for _, sg := range out.SecurityGroups {
		if aws.StringValue(sg.GroupName) == "default" {
			continue // never delete a VPC's default SG
		}
		id := aws.StringValue(sg.GroupId)
		if _, err := ec2c.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)}); err != nil {
			rep.errf("DeleteSecurityGroup %s: %w", id, err)
			continue
		}
		rep.deleted("securitygroup", id)
	}
}

func sweepSubnets(ec2c ec2iface.EC2API, runID string, rep *SweepReport) {
	out, err := ec2c.DescribeSubnets(&ec2.DescribeSubnetsInput{Filters: runFilter(runID)})
	if err != nil {
		rep.errf("DescribeSubnets: %w", err)
		return
	}
	for _, sn := range out.Subnets {
		id := aws.StringValue(sn.SubnetId)
		if _, err := ec2c.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(id)}); err != nil {
			rep.errf("DeleteSubnet %s: %w", id, err)
			continue
		}
		rep.deleted("subnet", id)
	}
}

func sweepKeyPairs(ec2c ec2iface.EC2API, runID string, rep *SweepReport) {
	out, err := ec2c.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{Filters: runFilter(runID)})
	if err != nil {
		rep.errf("DescribeKeyPairs: %w", err)
		return
	}
	for _, kp := range out.KeyPairs {
		name := aws.StringValue(kp.KeyName)
		if _, err := ec2c.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(name)}); err != nil {
			rep.errf("DeleteKeyPair %s: %w", name, err)
			continue
		}
		rep.deleted("keypair", name)
	}
}

// sweepLoadBalancers deletes ELBv2 load balancers carrying the run tag. ELBv2
// Describe has no tag filter, so it lists then matches via DescribeTags.
func sweepLoadBalancers(elbc elbv2iface.ELBV2API, runID string, rep *SweepReport) {
	if elbc == nil {
		return
	}
	out, err := elbc.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{})
	if err != nil {
		rep.errf("DescribeLoadBalancers: %w", err)
		return
	}
	var arns []string
	for _, lb := range out.LoadBalancers {
		arns = append(arns, aws.StringValue(lb.LoadBalancerArn))
	}
	for _, arn := range arns {
		tags, terr := elbc.DescribeTags(&elbv2.DescribeTagsInput{ResourceArns: []*string{aws.String(arn)}})
		if terr != nil {
			rep.errf("DescribeTags %s: %w", arn, terr)
			continue
		}
		if !elbHasRunTag(tags, runID) {
			continue
		}
		if _, err := elbc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(arn)}); err != nil {
			rep.errf("DeleteLoadBalancer %s: %w", arn, err)
			continue
		}
		rep.deleted("loadbalancer", arn)
	}
}

func elbHasRunTag(out *elbv2.DescribeTagsOutput, runID string) bool {
	for _, td := range out.TagDescriptions {
		for _, tag := range td.Tags {
			if aws.StringValue(tag.Key) == runTagKey && aws.StringValue(tag.Value) == runID {
				return true
			}
		}
	}
	return false
}
