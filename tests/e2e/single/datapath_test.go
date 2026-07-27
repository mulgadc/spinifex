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

// sgDatapathRevokeRounds returns how many times the revoke/re-authorize
// ingress round-trip repeats against the same client/target pair,
// overridable via SPINIFEX_SGDATAPATH_REVOKE_ROUNDS. Defaults to 1 — a
// single cycle, matching the original standalone test's behaviour; bump it
// for a soak run that repeatedly flips the sg-to-sg ACL to catch propagation
// flakiness that a single cycle would not surface.
func sgDatapathRevokeRounds() int {
	return envPositiveIntOr("SPINIFEX_SGDATAPATH_REVOKE_ROUNDS", 1)
}

// runSGPolicyDatapath merges two SG-policy checks that used to boot their own
// instance pair apiece — SG-to-SG datapath enforcement and same-default-SG
// east-west connectivity — around one shared client/target pair, cutting
// four boots to two.
//
// Stage order and gating:
//   - Setup (create SGs, launch client+target) is an unconditional
//     prerequisite for everything below it; any failure here is fatal in the
//     ordinary Go-test sense (require), matching the original.
//   - PortGroupMembership confirms OVN control-plane state (port_group +
//     address_set) and waits for client SSH. Every later stage depends on a
//     working SSH session, so its failure aborts the rest of the scenario
//     rather than let four more stages time out for the same reason.
//   - AllowedTraffic proves the sg-to-sg 8080 rule actually passes traffic.
//     DeniedTraffic (client -> target:22) checks a different port and an
//     unrelated rule, so it runs regardless of AllowedTraffic's outcome — a
//     real, independent signal either way. But the revoke/restore round-trip
//     below is only meaningful against a *proven* working baseline: if
//     AllowedTraffic never actually worked, "traffic is now blocked after
//     revoke" would trivially and misleadingly pass for the wrong reason. So
//     AllowedTraffic gates the revoke rounds.
//   - Each revoke round's restore half only depends on the revoke API call
//     having actually removed the rule (tracked separately from the
//     ICMP-style "verify blocked" assertion) — a flaky drop-detection
//     shouldn't stop the restore half from being independently verified.
//   - SameSGComms runs last and only *adds* the default SG to both ENIs via
//     ModifyNetworkInterfaceAttribute; see its own comment for why the
//     ordering there is load-bearing.
//
// OVN gated — skipped on dev laptops without ovn-nbctl/sudo. Maps to
// run-e2e.sh ~3188-3442 plus the former standalone same-default-SG check.
func runSGPolicyDatapath(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Security Group Policy on a Real Datapath (OVN)")
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
	require.NotEmpty(t, def.SGID, "default SG ID required")

	// --- Setup: create client-sg + target-sg, launch client-vm + target-vm ---

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
	// no SSH ingress — verified later by the denied-traffic stage.
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

	// Launch client-vm and target-vm in the default subnet so both get public
	// IPs (MapPublicIpOnLaunch=true). target's HTTP server is started via
	// cloud-init user-data — target-sg has no SSH ingress, so the test runner
	// cannot ssh into it directly, only nested-ssh from client.
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

	clientHost, clientPort := harness.InstancePublicSSHHost(t, clientInst)
	clientTgt := harness.SSHTarget{User: "ubuntu", Host: clientHost, Port: clientPort, KeyPath: keyPath}
	curlCmd := fmt.Sprintf("curl -sS -o /dev/null -m 5 http://%s:8080/", targetPriv)

	// --- PortGroupMembership: OVN control-plane state + client SSH ---

	portGroupOK := t.Run("PortGroupMembership", func(t *testing.T) {
		// ovn-nbctl port_group membership — confirm each LSP joined the
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
	})
	if !portGroupOK {
		t.Fatalf("PortGroupMembership stage failed; skipping every later stage that depends on client SSH")
	}

	// --- AllowedTraffic: client -> target:8080 must succeed ---

	allowedOK := t.Run("AllowedTraffic", func(t *testing.T) {
		// Retry to give target's cloud-init time to start python3 -m http.server.
		// Bash uses up to 30 attempts at 2s — keep the same outer budget.
		harness.Step(t, "8e-4 allowed traffic client -> target:%s:8080", targetPriv)
		harness.EventuallyErr(t, func() error {
			out, err := runSSHCombined(clientTgt, curlCmd)
			if err != nil {
				return fmt.Errorf("client -> target:8080 failed: %w (out=%q)", err, out)
			}
			return nil
		}, 60*time.Second, 2*time.Second)
		harness.Detail(t, "step4", "allowed_traffic_ok")
	})

	// --- DeniedTraffic: client -> target:22 must fail ---
	//
	// Independent of AllowedTraffic: a different port and an unrelated rule
	// (target-sg has no SSH ingress at all), so it runs and reports on its own
	// merits even if the 8080 path above failed.
	t.Run("DeniedTraffic", func(t *testing.T) {
		harness.Step(t, "8e-5 denied traffic client -> target:22 (no SSH ingress)")
		if _, err := runSSHCombined(clientTgt, fmt.Sprintf("nc -z -w 5 %s 22", targetPriv)); err == nil {
			t.Fatalf("FAIL: client reached target:22 — default-deny ACL not enforced")
		}
		harness.Detail(t, "step5", "denied_traffic_ok")
	})

	// --- Revoke / re-authorize round-trip ---
	//
	// Only meaningful against a proven-working baseline: if AllowedTraffic
	// never actually got through, "blocked after revoke" would trivially and
	// misleadingly pass for the wrong reason.
	if !allowedOK {
		t.Fatalf("AllowedTraffic never succeeded; skipping the revoke/restore round-trip since it would have nothing to detect a change from")
	}

	rounds := sgDatapathRevokeRounds()
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("Round%d", round), func(t *testing.T) {
			// The synchronous vpc.update-sg RequestEvent contract makes
			// propagation to OVN immediate — no propagation sleep needed.
			// Mirrors run-e2e.sh:3408.
			revokeMutationOK := true
			revokeOK := t.Run("RevokeAndVerifyBlocked", func(t *testing.T) {
				harness.Step(t, "8e-6 revoke target-sg ingress, retest (sync RequestEvent contract)")
				_, err := fix.AWS.EC2.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
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
				if err != nil {
					revokeMutationOK = false
				}
				require.NoError(t, err, "revoke-security-group-ingress tcp/8080 from %s", clientSG)

				// Fresh TCP connection — conntrack does not affect new
				// connections. Bash treats a single curl success as failure.
				harness.Step(t, "8e-6 verify client -> target:8080 now blocked")
				if _, err := runSSHCombined(clientTgt, curlCmd); err == nil {
					t.Fatalf("FAIL: client still reached target:8080 after revoke — propagation not immediate")
				}
				harness.Detail(t, "step6", "revoke_blocked_ok")
			})
			_ = revokeOK

			if !revokeMutationOK {
				t.Fatalf("revoke-security-group-ingress mutation failed; skipping re-authorize since the rule was never removed")
			}

			// Goes beyond the original bash Phase 8e (which stops at revoke)
			// to round-trip the ACL state machine and prove the rule lifecycle
			// is symmetric. Without this a one-way regression (revoke works,
			// re-add silently no-ops) would slip past.
			t.Run("ReauthorizeAndVerifyRestored", func(t *testing.T) {
				harness.Step(t, "8e-7 re-authorize target-sg ingress, verify traffic restored")
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
				require.NoError(t, err, "re-authorize tcp/8080 from %s", clientSG)

				harness.EventuallyErr(t, func() error {
					out, err := runSSHCombined(clientTgt, curlCmd)
					if err != nil {
						return fmt.Errorf("client -> target:8080 still blocked after re-add: %w (out=%q)", err, out)
					}
					return nil
				}, 30*time.Second, 1*time.Second)
				harness.Detail(t, "step7", "restore_ok")
			})
		})
	}

	// --- SameSGComms: east-west over the default SG's self-reference rule ---

	t.Run("SameSGComms", func(t *testing.T) {
		// This stage only ever ADDS the default SG to each ENI, and runs last,
		// after every SG-to-SG allow/deny assertion above has already
		// completed and recorded its own result. The default SG's ingress
		// rule is `-1/-1` from same-group members (see
		// createDefaultSecurityGroupInternal) — i.e. it admits ALL traffic
		// between co-members, not just ICMP. Joining it any earlier would
		// silently defeat the DeniedTraffic assertion above (client and
		// target would both gain a blanket allow via default-SG
		// co-membership, independent of target-sg's rules), so the ordering
		// here is load-bearing, not incidental.
		harness.Step(t, "join client-vm + target-vm to the default SG for same-SG comms")
		addSecurityGroup(t, fix, clientENI, []string{clientSG}, def.SGID)
		addSecurityGroup(t, fix, targetENI, []string{targetSG}, def.SGID)

		harness.Step(t, "ping %s (%s) from %s via default-SG self-ingress", targetID, targetPriv, clientID)
		out, converged := pingConverged(clientTgt, targetPriv, 45*time.Second)
		require.Truef(t, converged,
			"intra-default-SG east-west %s -> %s never reached 0%% loss within 45s; "+
				"ARP/L2 datapath unreachable across the default-SG self-ingress\n%s",
			clientID, targetID, out)
	})
}

// addSecurityGroup replaces eniID's security-group list with currentSGIDs
// plus addSGID. ModifyNetworkInterfaceAttribute's Groups field is a full
// replacement, not an additive patch (matches the real EC2 API), so the
// caller's existing membership must be restated alongside the new one.
func addSecurityGroup(t *testing.T, fix *Fixture, eniID string, currentSGIDs []string, addSGID string) {
	t.Helper()
	groups := make([]*string, 0, len(currentSGIDs)+1)
	for _, id := range currentSGIDs {
		groups = append(groups, aws.String(id))
	}
	groups = append(groups, aws.String(addSGID))
	_, err := fix.AWS.EC2.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniID),
		Groups:             groups,
	})
	require.NoError(t, err, "modify-network-interface-attribute %s: add SG %s", eniID, addSGID)
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
