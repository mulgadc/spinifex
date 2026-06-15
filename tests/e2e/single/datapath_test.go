//go:build e2e

package single

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// targetUserData starts python3 -m http.server 8080 via systemd-run so the
// HTTP server fully detaches from cloud-init's process group. `nohup ... &`
// alone races with cloud-init exit and the server can get killed. Mirrors
// run-e2e.sh:3243-3248 (Phase 8e step 2). Passed as base64 — aws-sdk-go
// expects UserData already base64-encoded, unlike the AWS CLI which encodes
// plaintext for you.
const targetUserData = `#!/bin/bash
systemd-run --unit=sge-http --description="Phase 8e HTTP server" \
    /usr/bin/python3 -m http.server 8080 --bind 0.0.0.0
`

// runSGToSGDatapath validates the closed loop from RunInstances
// (--security-group-ids) -> ENI -> vpcd -> OVN port group + ACL -> datapath
// drop. Creates two SGs (client / target) with an SG-to-SG ingress rule for
// tcp/8080, launches one VM in each, then exercises:
//
//  1. ovn-nbctl port_group membership for both LSPs (catches Phase 4
//     membership regressions independently of packet timing).
//  2. ovn-sbctl address_set <pg>_ip4 contains the client's private IP
//     (auto-derived by northd from port_group membership; lives in SB).
//  3. Allowed traffic — client -> target:8080 over private IP (must succeed).
//  4. Denied traffic — client -> target:22 (target SG has no SSH ingress).
//  5. Revoke target SG's tcp/8080 ingress -> traffic immediately blocked
//     (sync vpc.update-sg RequestEvent contract, no propagation sleep).
//
// OVN gated — skipped on dev laptops without ovn-nbctl/sudo. Maps to
// run-e2e.sh ~3188-3442.
func runSGToSGDatapath(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Security Group SG-to-SG Datapath (OVN)")
	harness.SkipIfNoOVN(t)
	requireSSHHealthy(t)

	// Bootstrap every prereq up front. runSGEInstance / primaryENI use
	// `fix.AMIID / InstanceType / KeyName / KeyPath` indirectly; resolve
	// once and pass into the local helpers.
	_ = needAMI(t, fix)
	_, _ = needInstanceTypeArch(t, fix)
	_, keyPath := needKeyPair(t, fix)

	def := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, def.VPCID, "default VPC ID required")
	require.NotEmpty(t, def.SubnetID, "default subnet ID required")

	// Step 1: Create client-sg and target-sg.
	harness.Step(t, "8e-1 create sge-client + sge-target security groups")
	clientSG := createSG(t, fix, def.VPCID, "sge-client", "Phase 8e client SG (SSH ingress from anywhere)")
	targetSG := createSG(t, fix, def.VPCID, "sge-target", "Phase 8e target SG (TCP/8080 ingress from client-sg only)")

	// Pre-register SG cleanup BEFORE instance cleanup so the LIFO order runs:
	// terminate instances -> delete SGs. Otherwise delete-security-group fails
	// because the SG still references live ENIs.
	t.Cleanup(func() {
		// Best-effort: SG may already be gone if the test reached its own
		// cleanup section, but on early failure these run to free state.
		if _, err := fix.AWS.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(targetSG),
		}); err != nil && !harness.ErrorCodeIs(err, "InvalidGroup.NotFound") {
			t.Logf("WARNING: cleanup delete %s: %v", targetSG, err)
		}
		if _, err := fix.AWS.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(clientSG),
		}); err != nil && !harness.ErrorCodeIs(err, "InvalidGroup.NotFound") {
			t.Logf("WARNING: cleanup delete %s: %v", clientSG, err)
		}
	})

	harness.Detail(t, "client_sg", clientSG, "target_sg", targetSG)

	// SSH ingress on client SG so the test runner can reach it. Target SG has
	// no SSH ingress — verified later by Step 5.
	harness.AuthorizeSSHIngress(t, fix.AWS, clientSG)

	// SG-to-SG ingress via UserIdGroupPair (VPC-form; --source-group shorthand
	// is EC2-Classic only). Mirrors run-e2e.sh:3217-3221.
	harness.Step(t, "8e-1 authorize target-sg ingress tcp/8080 from %s", clientSG)
	_, err := fix.AWS.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(targetSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(8080),
			ToPort:     aws.Int64(8080),
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{
				GroupId: aws.String(clientSG),
			}},
		}},
	})
	require.NoError(t, err, "authorize-security-group-ingress tcp/8080 from %s", clientSG)

	// Step 2: Launch client-vm and target-vm in the default subnet so both get
	// public IPs (MapPublicIpOnLaunch=true). target's HTTP server is started
	// via cloud-init user-data — target-sg has no SSH ingress, so the test
	// runner cannot ssh into it directly, only nested-ssh from client.
	harness.Step(t, "8e-2 launch client-vm + target-vm")

	clientID := runSGEInstance(t, fix, def.SubnetID, clientSG, "" /* no user-data */)
	targetID := runSGEInstance(t, fix, def.SubnetID, targetSG, targetUserData)

	// Pre-register instance cleanup BEFORE the running wait so a t.Fatal
	// mid-wait still tears them down.
	t.Cleanup(func() {
		_, err := fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: aws.StringSlice([]string{clientID, targetID}),
		})
		if err != nil && !harness.ErrorCodeIs(err, "InvalidInstanceID.NotFound") {
			t.Logf("WARNING: cleanup terminate %s, %s: %v", clientID, targetID, err)
			return
		}
		// Wait for terminated so DeleteSecurityGroup (registered earlier,
		// runs after this) doesn't trip on live ENIs.
		for _, id := range []string{clientID, targetID} {
			harness.WaitForInstanceState(t, fix.AWS, id, "terminated",
				harness.WithTimeout(2*time.Minute), harness.WithPoll(2*time.Second))
		}
	})

	clientInst := harness.WaitForInstanceState(t, fix.AWS, clientID, "running")
	targetInst := harness.WaitForInstanceState(t, fix.AWS, targetID, "running")

	clientPriv := aws.StringValue(clientInst.PrivateIpAddress)
	targetPriv := aws.StringValue(targetInst.PrivateIpAddress)
	require.NotEmpty(t, clientPriv, "client-vm has no PrivateIpAddress")
	require.NotEmpty(t, targetPriv, "target-vm has no PrivateIpAddress")

	clientENI := primaryENI(t, clientInst)
	targetENI := primaryENI(t, targetInst)
	harness.Detail(t,
		"client_priv", clientPriv, "client_eni", clientENI,
		"target_priv", targetPriv, "target_eni", targetENI,
	)

	// Step 3: ovn-nbctl port_group membership — confirm each LSP joined the
	// expected port group. Spinifex maps `sg-XXXX` -> `sg_XXXX` because OVN
	// port group names use [_a-zA-Z0-9] only. LSP name format is
	// `port-<eniID>` (run-e2e.sh:3299-3300).
	harness.Step(t, "8e-3 ovn-nbctl port_group membership")
	clientPG := strings.ReplaceAll(clientSG, "-", "_")
	targetPG := strings.ReplaceAll(targetSG, "-", "_")
	clientLSP := "port-" + clientENI
	targetLSP := "port-" + targetENI

	// WaitForPortGroupMember polls NB until the LSP UUID appears in the
	// port_group's ports column. Bounds the race between RunInstances
	// returning and OVN flow install — northd propagation is normally
	// sub-second but tests run on busy nodes.
	harness.WaitForPortGroupMember(t, clientPG, clientLSP)
	harness.WaitForPortGroupMember(t, targetPG, targetLSP)
	harness.Detail(t, "client_pg", clientPG, "target_pg", targetPG)

	// Confirm client's private IP made it into the `<pg>_ip4` address set so
	// target-sg's SG-to-SG match expression resolves. The <pg>_ip4 / <pg>_ip6
	// sets are auto-derived by ovn-northd from port_group membership and live
	// in SB, not NB. northd is async so poll briefly to ride out the
	// post-join propagation window. Mirrors run-e2e.sh:3334-3349.
	harness.Step(t, "8e-3 ovn-sbctl address_set %s_ip4 contains %s", clientPG, clientPriv)
	addrSetName := clientPG + "_ip4"
	harness.EventuallyErr(t, func() error {
		addrs := harness.OvnSbctl(t, "--no-leader-only", "--bare", "--columns=addresses",
			"find", "address_set", "name="+addrSetName)
		if strings.Contains(addrs, clientPriv) {
			return nil
		}
		return fmt.Errorf("client private IP %s missing from address_set %s (addresses=%s)",
			clientPriv, addrSetName, addrs)
	}, 10*time.Second, 1*time.Second)

	// Wait for client SSH to become reachable. The test runner uses the
	// public IP / hostfwd to reach client-vm; the nested SSH below is just
	// `ssh <client> 'curl <target>:8080'`. Target-vm has no SSH ingress on
	// its own SG so we never connect to it directly.
	clientHost, clientPort := harness.InstancePublicSSHHost(t, clientInst)
	clientTgt := harness.SSHTarget{User: "ec2-user", Host: clientHost, Port: clientPort, KeyPath: keyPath}
	harness.Step(t, "8e-3 wait for client-vm SSH at %s:%d", clientHost, clientPort)
	// Non-fatal probe so a timeout dumps the guest console + OVN/datapath
	// state before Fatal. A full 2min unreachable window on a fresh public-IP
	// VM is the flake signature; capture it from CI artifacts alone.
	if !trySSHReady(clientHost, clientPort, keyPath, 2*time.Minute) {
		harness.DumpVPCFlowDiagnostics(t, fix.AWS, clientID,
			fmt.Sprintf("8e-3 client-vm SSH timeout — vpc=%s sg=%s pub=%s", def.VPCID, clientSG, clientHost),
			harness.VPCDiagnosticsOpts{
				ExternalIP:  clientHost,
				LogicalIP:   clientPriv,
				ArtifactDir: fix.ArtifactDir(t),
			})
		t.Fatalf("client-vm SSH %s:%d never became reachable within 2min (see diagnostics above)", clientHost, clientPort)
	}

	// Step 4: Allowed traffic — client -> target:8080 must succeed.
	// Retry to give target's cloud-init time to start python3 -m http.server.
	// Bash uses up to 30 attempts at 2s — keep the same outer budget.
	harness.Step(t, "8e-4 allowed traffic client -> target:%s:8080", targetPriv)
	curlCmd := fmt.Sprintf("curl -sS -o /dev/null -m 5 http://%s:8080/", targetPriv)
	harness.EventuallyErr(t, func() error {
		out, err := runSSHCombined(clientTgt, curlCmd)
		if err != nil {
			return fmt.Errorf("client -> target:8080 failed: %w (out=%q)", err, out)
		}
		return nil
	}, 60*time.Second, 2*time.Second)
	harness.Detail(t, "step4", "allowed_traffic_ok")

	// Step 5: Denied traffic — client -> target:22 must fail. target-sg has
	// no SSH ingress; default-deny ACL must drop. `nc -z -w 5` exits 0 on
	// connect, non-zero on timeout/refuse — we expect non-zero.
	harness.Step(t, "8e-5 denied traffic client -> target:22 (no SSH ingress)")
	if _, err := runSSHCombined(clientTgt, fmt.Sprintf("nc -z -w 5 %s 22", targetPriv)); err == nil {
		t.Fatalf("FAIL: client reached target:22 — default-deny ACL not enforced")
	}
	harness.Detail(t, "step5", "denied_traffic_ok")

	// Step 6: Revoke target-sg's 8080 rule and retest immediately. The
	// synchronous vpc.update-sg RequestEvent contract makes propagation to
	// OVN immediate — no propagation sleep needed. Mirrors run-e2e.sh:3408.
	harness.Step(t, "8e-6 revoke target-sg ingress, retest (sync RequestEvent contract)")
	_, err = fix.AWS.EC2.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(targetSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(8080),
			ToPort:     aws.Int64(8080),
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{
				GroupId: aws.String(clientSG),
			}},
		}},
	})
	require.NoError(t, err, "revoke-security-group-ingress tcp/8080 from %s", clientSG)

	// Fresh TCP connection — conntrack does not affect new connections.
	// Bash treats a single curl success as failure; allow a tight poll window
	// to detect lingering reachability without flaking on the first attempt
	// catching an in-flight flow flush.
	harness.Step(t, "8e-6 verify client -> target:8080 now blocked")
	if _, err := runSSHCombined(clientTgt, curlCmd); err == nil {
		t.Fatalf("FAIL: client still reached target:8080 after revoke — propagation not immediate")
	}
	harness.Detail(t, "step6", "revoke_blocked_ok")

	// Step 7: Re-authorize, confirm traffic restored. Goes beyond the bash
	// Phase 8e (bash stops at revoke) to round-trip the ACL state machine
	// and prove the rule lifecycle is symmetric. Without this step a one-way
	// regression (revoke works, re-add silently no-ops) would slip past.
	harness.Step(t, "8e-7 re-authorize target-sg ingress, verify traffic restored")
	_, err = fix.AWS.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(targetSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(8080),
			ToPort:     aws.Int64(8080),
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{
				GroupId: aws.String(clientSG),
			}},
		}},
	})
	require.NoError(t, err, "re-authorize tcp/8080 from %s", clientSG)

	harness.EventuallyErr(t, func() error {
		out, err := runSSHCombined(clientTgt, curlCmd)
		if err != nil {
			return fmt.Errorf("client -> target:8080 still blocked after re-add: %w (out=%q)", err, out)
		}
		return nil
	}, 30*time.Second, 1*time.Second)
	harness.Detail(t, "step7", "restore_ok")
}

// createSG creates a security group in the default VPC and returns its ID.
// Failures are fatal — the rest of the phase depends on both SGs existing.
func createSG(t *testing.T, fix *Fixture, vpcID, name, desc string) string {
	t.Helper()
	out, err := fix.AWS.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(name),
		Description: aws.String(desc),
		VpcId:       aws.String(vpcID),
	})
	require.NoError(t, err, "create-security-group %s", name)
	id := aws.StringValue(out.GroupId)
	require.NotEmpty(t, id, "CreateSecurityGroup returned empty GroupId")
	return id
}

// runSGEInstance launches a single instance in subnetID bound to sgID.
// userData may be empty; when set it is base64-encoded for the SDK (the
// AWS CLI does this for you, the SDK does not). Returns the instance ID.
//
// Bypasses EnsureInstance on purpose: phase 8e's client/target VMs are
// test subjects, not memoized prerequisites — each phase8e run must get
// fresh VMs (different SGs, fresh ENI/private-IP for the address-set
// assertion). Memoizing across runs would defeat the test.
func runSGEInstance(t *testing.T, fix *Fixture, subnetID, sgID, userData string) string {
	t.Helper()
	amiID := needAMI(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, _ := needKeyPair(t, fix)
	in := &ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(subnetID),
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
		SecurityGroupIds: []*string{aws.String(sgID)},
	}
	if userData != "" {
		in.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(userData)))
	}
	out, err := fix.AWS.EC2.RunInstances(in)
	require.NoError(t, err, "run-instances sg=%s", sgID)
	require.NotEmpty(t, out.Instances, "run-instances sg=%s returned no Instances", sgID)
	id := aws.StringValue(out.Instances[0].InstanceId)
	require.NotEmpty(t, id, "run-instances sg=%s returned empty InstanceId", sgID)
	return id
}

// primaryENI returns the NetworkInterfaceId of an instance's first ENI.
// t.Fatal if the instance has no ENI — every running EC2 instance must.
func primaryENI(t *testing.T, inst *ec2.Instance) string {
	t.Helper()
	if len(inst.NetworkInterfaces) == 0 {
		t.Fatalf("instance %s has no NetworkInterfaces", aws.StringValue(inst.InstanceId))
	}
	eni := aws.StringValue(inst.NetworkInterfaces[0].NetworkInterfaceId)
	if eni == "" {
		t.Fatalf("instance %s primary ENI has empty NetworkInterfaceId", aws.StringValue(inst.InstanceId))
	}
	return eni
}
