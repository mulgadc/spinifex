//go:build e2e

package harness

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// SSHTarget is the minimal addressing tuple needed by LsblkRootGiB. The
// existing harness.SSH interface requires a Cluster.Node, but the single-host
// e2e suite addresses individual instances by (host, port, key) discovered at
// runtime — those don't fit a Cluster. Defined here rather than in ssh.go to
// keep the production SSH transport untouched.
type SSHTarget struct {
	User    string
	Host    string
	Port    int
	KeyPath string
}

// InstancePublicSSHHost returns (host, 22) for an SSH connection to inst's
// public IP.
//
// There is deliberately NO qemu-hostfwd fallback. hostfwd (127.0.0.1 ->
// guest:22) is a dev_networking shortcut that bypasses the OVN datapath
// entirely — no SG ACL, no IGW, no SNAT/DNAT — so a test that reached a guest
// through it would validate nothing real and mask exactly the networking
// regressions these tests exist to catch. An instance with no public IP is a
// hard failure: the routing it depends on is broken or unconfigured.
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

// LsblkRootGiB SSHes into the VM and returns the root disk size in GiB,
// cross-checking the value the API reports against the guest's view.
//
// Equivalent of run-e2e.sh's lsblk pipeline (findmnt → lsblk PKNAME → lsblk
// -b -d). Returns the GiB rounded down (same math as bash: bytes / 1<<30).
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

// DiscoverDefaultVPC returns (vpcID, defaultSGID, defaultSubnetID) for the
// account's default VPC. t.Fatal if the default VPC is missing — the suite
// can't run without one, and silently falling back masks the real failure
// (daemon didn't create the default VPC on account bootstrap).
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
	// Fall back to the first subnet if none is marked DefaultForAz — pseudo
	// multinode skips the marker, and callers just need a usable subnet ID.
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

// EnsureDefaultSGOpen authorizes tcp/22 + ICMP on the default VPC's default
// SG. AWS default SGs only admit same-SG members; e2e probes come from the
// test runner's external IP, so without this every SSH/ping hits the OVN
// port-group ACL drop. Mirrors the Phase-5 block in run-e2e.sh.
func EnsureDefaultSGOpen(t *testing.T, c *AWSClient) {
	t.Helper()
	_, sgID, _ := DiscoverDefaultVPC(t, c)
	AuthorizeSSHIngress(t, c, sgID)
	AuthorizeICMPIngress(t, c, sgID)
}

// asErr is a thin errors.As that lets the helper stay testify-free.
func asErr(err error, target *awserr.Error) bool {
	if a, ok := err.(awserr.Error); ok {
		*target = a
		return true
	}
	return false
}
