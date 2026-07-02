//go:build e2e

package harness

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// SSHTarget is the minimal addressing tuple used by LsblkRootGiB. Unlike
// harness.SSH / Cluster.Node, instances here are addressed by (host, port, key)
// discovered at runtime, which doesn't fit the Cluster model.
type SSHTarget struct {
	User    string
	Host    string
	Port    int
	KeyPath string
}

// InstancePublicSSHHost returns (host, 22) for an SSH connection to inst's
// public IP. No qemu-hostfwd fallback: hostfwd bypasses the OVN datapath (no
// SG ACL, no IGW, no SNAT/DNAT), masking the networking regressions these
// tests exist to catch. No public IP is a hard failure.
func InstancePublicSSHHost(t *testing.T, inst *ec2.Instance) (string, int) {
	t.Helper()
	if inst == nil {
		t.Fatalf("InstancePublicSSHHost: nil instance")
	}
	pub := aws.StringValue(inst.PublicIpAddress)
	if pub == "" || pub == "None" {
		t.Fatalf("InstancePublicSSHHost: instance %s has no public IP; "+
			"qemu-hostfwd fallback is disabled (it bypasses the OVN datapath)",
			aws.StringValue(inst.InstanceId))
	}
	return pub, 22
}

// InstancePrivateIP returns inst's primary private IPv4, or "" when unset or
// unresolvable. Used to populate VPCDiagnosticsOpts.LogicalIP so the datapath
// captures grep both halves of the NAT translation. Non-fatal — it feeds a
// best-effort diagnostic, so a describe miss logs and returns "".
func InstancePrivateIP(t *testing.T, c *AWSClient, instanceID string) string {
	t.Helper()
	out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	if err != nil {
		t.Logf("InstancePrivateIP: describe %s: %v", instanceID, err)
		return ""
	}
	for _, r := range out.Reservations {
		for _, in := range r.Instances {
			return aws.StringValue(in.PrivateIpAddress)
		}
	}
	return ""
}

// LsblkRootGiB SSHes into the VM and returns the root disk size in GiB
// (bytes / 1<<30, rounded down) by running findmnt + lsblk in the guest.
func LsblkRootGiB(t *testing.T, tgt SSHTarget) int {
	t.Helper()
	cmd := `SRC=$(findmnt -n -o SOURCE /); PKN=$(lsblk -n -o PKNAME "$SRC" 2>/dev/null | head -1); DEV=${PKN:-$(basename "$SRC")}; lsblk -b -d -n -o SIZE "/dev/$DEV"`
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User + "@" + tgt.Host,
		cmd,
	}
	var stdout, stderr bytes.Buffer
	sshCmd := exec.Command("ssh", args...)
	sshCmd.Stdout = &stdout
	sshCmd.Stderr = &stderr
	if err := sshCmd.Run(); err != nil {
		t.Fatalf("ssh lsblk %s@%s:%d failed: %v\nstderr: %s", tgt.User, tgt.Host, tgt.Port, err, stderr.String())
	}
	raw := strings.TrimSpace(stdout.String())
	bytesN, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("ssh lsblk %s@%s:%d: parse %q: %v", tgt.User, tgt.Host, tgt.Port, raw, err)
	}
	return int(bytesN / (1 << 30))
}

// GetSerialConsoleAccessEnabled returns the current account attribute value.
// Wraps ec2.GetSerialConsoleAccessStatus so callers don't have to deal with
// the pointer-bool field.
func GetSerialConsoleAccessEnabled(t *testing.T, c *AWSClient) bool {
	t.Helper()
	out, err := c.EC2.GetSerialConsoleAccessStatus(&ec2.GetSerialConsoleAccessStatusInput{})
	if err != nil {
		t.Fatalf("GetSerialConsoleAccessStatus: %v", err)
	}
	return aws.BoolValue(out.SerialConsoleAccessEnabled)
}

// EnableSerialConsoleAccess flips the account attribute on and returns the
// new value (should be true).
func EnableSerialConsoleAccess(t *testing.T, c *AWSClient) bool {
	t.Helper()
	out, err := c.EC2.EnableSerialConsoleAccess(&ec2.EnableSerialConsoleAccessInput{})
	if err != nil {
		t.Fatalf("EnableSerialConsoleAccess: %v", err)
	}
	return aws.BoolValue(out.SerialConsoleAccessEnabled)
}

// DisableSerialConsoleAccess flips the account attribute off and returns the
// new value (should be false).
func DisableSerialConsoleAccess(t *testing.T, c *AWSClient) bool {
	t.Helper()
	out, err := c.EC2.DisableSerialConsoleAccess(&ec2.DisableSerialConsoleAccessInput{})
	if err != nil {
		t.Fatalf("DisableSerialConsoleAccess: %v", err)
	}
	return aws.BoolValue(out.SerialConsoleAccessEnabled)
}

// DiscoverDefaultVPC returns (vpcID, defaultSGID, defaultSubnetID). Fatals if
// the default VPC is absent — its absence indicates a bootstrap failure.
func DiscoverDefaultVPC(t *testing.T, c *AWSClient) (vpcID, sgID, subnetID string) {
	t.Helper()
	vpcs, err := c.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{{Name: aws.String("is-default"), Values: []*string{aws.String("true")}}},
	})
	if err != nil {
		t.Fatalf("describe-vpcs: %v", err)
	}
	if len(vpcs.Vpcs) == 0 {
		t.Fatalf("no default VPC found")
	}
	vpcID = aws.StringValue(vpcs.Vpcs[0].VpcId)

	sgs, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
			{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
		},
	})
	if err != nil {
		t.Fatalf("describe-security-groups: %v", err)
	}
	if len(sgs.SecurityGroups) == 0 {
		t.Fatalf("default SG missing for VPC %s", vpcID)
	}
	sgID = aws.StringValue(sgs.SecurityGroups[0].GroupId)

	subnets, err := c.EC2.DescribeSubnets(&ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}}},
	})
	if err != nil {
		t.Fatalf("describe-subnets: %v", err)
	}
	for _, s := range subnets.Subnets {
		if aws.BoolValue(s.DefaultForAz) {
			subnetID = aws.StringValue(s.SubnetId)
			break
		}
	}
	// Fall back to the first subnet if none is marked DefaultForAz; callers
	// just need a usable subnet ID.
	if subnetID == "" && len(subnets.Subnets) > 0 {
		subnetID = aws.StringValue(subnets.Subnets[0].SubnetId)
	}
	return vpcID, sgID, subnetID
}

// AuthorizeSSHIngress idempotently authorizes tcp/22 ingress from 0.0.0.0/0
// on sgID. InvalidPermission.Duplicate is treated as success — the rule was
// already in place on a re-run.
func AuthorizeSSHIngress(t *testing.T, c *AWSClient, sgID string) {
	t.Helper()
	_, err := c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(22),
			ToPort:     aws.Int64(22),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}},
	})
	if err == nil {
		return
	}
	var aerr awserr.Error
	if asErr(err, &aerr) && aerr.Code() == "InvalidPermission.Duplicate" {
		return
	}
	t.Fatalf("authorize-security-group-ingress tcp/22 on %s: %v", sgID, err)
}

// AuthorizeICMPIngress idempotently authorizes ICMP (all types) ingress from
// 0.0.0.0/0 on sgID. Same Duplicate-tolerant contract as AuthorizeSSHIngress.
func AuthorizeICMPIngress(t *testing.T, c *AWSClient, sgID string) {
	t.Helper()
	_, err := c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("icmp"),
			FromPort:   aws.Int64(-1),
			ToPort:     aws.Int64(-1),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}},
	})
	if err == nil {
		return
	}
	var aerr awserr.Error
	if asErr(err, &aerr) && aerr.Code() == "InvalidPermission.Duplicate" {
		return
	}
	t.Fatalf("authorize-security-group-ingress icmp on %s: %v", sgID, err)
}

// EnsureDefaultSGOpen authorizes tcp/22 + ICMP on the default SG. Default SGs
// only admit same-SG members; without this, runner-originated SSH/ping is dropped
// by the OVN port-group ACL.
func EnsureDefaultSGOpen(t *testing.T, c *AWSClient) {
	t.Helper()
	_, sgID, _ := DiscoverDefaultVPC(t, c)
	AuthorizeSSHIngress(t, c, sgID)
	AuthorizeICMPIngress(t, c, sgID)
}

// AttachVolumeWait attaches volID to instanceID at device and blocks until the
// volume reaches "in-use". Used by the data-durability tests where the attach
// must complete before the guest can format the disk.
func AttachVolumeWait(t *testing.T, c *AWSClient, volID, instanceID, device string) {
	t.Helper()
	_, err := c.EC2.AttachVolume(&ec2.AttachVolumeInput{
		VolumeId:   aws.String(volID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String(device),
	})
	if err != nil {
		t.Fatalf("attach-volume %s -> %s as %s: %v", volID, instanceID, device, err)
	}
	WaitForVolumeState(t, c, volID, ec2.VolumeStateInUse, WithPoll(500*time.Millisecond))
}

// DetachVolumeWait detaches volID and blocks until it reaches "available".
func DetachVolumeWait(t *testing.T, c *AWSClient, volID string) {
	t.Helper()
	_, err := c.EC2.DetachVolume(&ec2.DetachVolumeInput{VolumeId: aws.String(volID)})
	if err != nil {
		t.Fatalf("detach-volume %s: %v", volID, err)
	}
	WaitForVolumeState(t, c, volID, "available", WithPoll(500*time.Millisecond))
}

// RegisterVolumeTeardown best-effort force-detaches then deletes volID at test
// end. Non-fatal — it polls for "available" within a bounded window and never
// calls t.Fatal so a cleanup hiccup doesn't mask the test's real result.
func RegisterVolumeTeardown(t *testing.T, c *AWSClient, volID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = c.EC2.DetachVolume(&ec2.DetachVolumeInput{
			VolumeId: aws.String(volID),
			Force:    aws.Bool(true),
		})
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			out, err := c.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
				VolumeIds: []*string{aws.String(volID)},
			})
			if err != nil || len(out.Volumes) == 0 {
				return
			}
			if aws.StringValue(out.Volumes[0].State) == "available" {
				break
			}
			time.Sleep(2 * time.Second)
		}
		_, _ = c.EC2.DeleteVolume(&ec2.DeleteVolumeInput{VolumeId: aws.String(volID)})
	})
}

// asErr is a thin errors.As that lets the helper stay testify-free.
func asErr(err error, target *awserr.Error) bool {
	if a, ok := err.(awserr.Error); ok {
		*target = a
		return true
	}
	return false
}
