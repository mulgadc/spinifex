//go:build e2e

package single

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// IMDS E2E identifiers. The role/profile carry an imds-e2e- prefix so they
// never collide with the IAM-roles suite ("app-role"/"app-profile") or the
// STS suite ("sts-e2e-role").
const (
	imdsRoleName    = "imds-e2e-role"
	imdsProfileName = "imds-e2e-profile"

	// metaURL is the AWS-compatible link-local IMDS endpoint, served from the
	// host per-VPC via SO_BINDTODEVICE on the imds-h-<shortVpcID> veth.
	metaURL = "http://169.254.169.254"

	// imdsUserData is a no-op cloud-config carrying a unique marker so the
	// /latest/user-data round-trip can assert the body the guest sees matches
	// what RunInstances was given. A comment-only cloud-config is a valid empty
	// config, so it doesn't perturb boot.
	imdsUserData = "#cloud-config\n# imds-e2e-userdata-marker-7f3a91\n"
	imdsUDMarker = "imds-e2e-userdata-marker-7f3a91"

	// imdsProbeVPCCIDR / imdsProbeSubnetCIDR are shared by BOTH isolation VPCs.
	// Overlapping CIDRs across VPCs are legal (each VPC is its own routing
	// domain); using identical subnets makes IPAM hand the first VM in each the
	// SAME private IP (network+4, deterministic per ipam.go), which is exactly
	// the cross-VPC source-IP collision the isolation assertion needs.
	imdsProbeVPCCIDR    = "10.211.0.0/16"
	imdsProbeSubnetCIDR = "10.211.7.0/24"
)

// imdsCredDoc mirrors the AWS credential JSON served at
// /latest/meta-data/iam/security-credentials/<role>. Only the fields the test
// asserts on are decoded.
type imdsCredDoc struct {
	Code            string `json:"Code"`
	Type            string `json:"Type"`
	AccessKeyId     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
	AccountId       string `json:"AccountId"`
}

// runIMDS exercises the host-served IMDSv2 surface end-to-end against real guest
// VMs (docs/development/feature/imds-v1.md Step 11). It stands up two VPCs with
// identical subnet CIDRs and one VM in each so the two VMs share a private IP:
//
//   - VM X (profile-bound, user-data) drives the full surface: IMDSv2 token
//     issuance, the v2-only stance (tokenless / garbage-token GET → 401), the
//     metadata fields, the instance-role credential path + a wire round-trip
//     proving the ASIA creds resolve to the instance assumed-role ARN, the
//     /latest/user-data round-trip, and the DHCP option-121 guest route.
//   - The per-VPC datapath invariant: imds-port-<vpcID> is a localport LSP.
//   - Cross-VPC isolation: VM X and VM Y share the same source IP in different
//     VPCs, yet each IMDS returns its OWN instance-id — the load-bearing
//     (VPC-ID, source-IP) → ENI boundary (plan §Datapath-attested identity).
//
// OVN + SSH gated: the IMDS datapath needs ovn-controller to bind the localport
// per chassis, and every assertion runs via SSH into the guest.
func runIMDS(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — IMDSv2 Host-Served Instance Metadata")
	harness.SkipIfNoOVN(t)
	requireSSHHealthy(t)

	adminAccount := iamEnsureAdminAccountID(t, fix)
	_, keyPath := needKeyPair(t, fix)
	imdsEnsureRoleProfile(t, fix, adminAccount)

	// Two VPCs, identical subnet CIDR → the first VM in each gets the same IP.
	vpcX, subX, sgX := imdsEnsureProbeVPC(t, fix, imdsProbeVPCCIDR, imdsProbeSubnetCIDR)
	vpcY, subY, sgY := imdsEnsureProbeVPC(t, fix, imdsProbeVPCCIDR, imdsProbeSubnetCIDR)

	// VM X — profile-bound + user-data; drives the full surface.
	idX, privX, tgtX := imdsProbe(t, fix, keyPath, imdsVMSpec{
		subnetID:    subX,
		sgID:        sgX,
		profileName: imdsProfileName,
		userData:    imdsUserData,
	})
	// VM Y — isolation peer; no profile needed for the instance-id probe.
	idY, privY, tgtY := imdsProbe(t, fix, keyPath, imdsVMSpec{
		subnetID: subY,
		sgID:     sgY,
	})
	require.NotEqual(t, idX, idY, "the two probe VMs must be distinct instances")
	require.Equalf(t, privX, privY,
		"isolation premise: both first-in-subnet VMs must share a private IP "+
			"(IPAM is deterministic at network+4) — got %s vs %s", privX, privY)
	harness.Detail(t, "vm_x", idX, "vm_y", idY, "shared_priv", privX)

	// --- Per-VPC OVN datapath invariant: imds-port-<vpc> is a localport ------
	harness.Step(t, "ovn-nbctl imds-port-%s must be a localport LSP", vpcX)
	imdsLSP := "imds-port-" + vpcX // mirrors topology.IMDSPort (inlined, as datapath_test does)
	lspType := harness.OvnNbctl(t, "--no-leader-only", "--bare", "--columns=type",
		"find", "logical_switch_port", "name="+imdsLSP)
	require.Equalf(t, "localport", lspType,
		"imds-port LSP %s must be type=localport so every chassis self-serves IMDS "+
			"(a regular LSP binds one chassis and forces Geneve tunnelling)", imdsLSP)

	// --- Subnet-router proxy-ARP makes 169.254.169.254 reachable link-local --
	// IMDS reachability no longer depends on a DHCP option-121 route in the
	// guest; the subnet router LSP answers ARP for the IMDS address via OVN
	// options:arp_proxy, so DHCP and fully static guests reach it identically.
	harness.Step(t, "subnet router LSP rtr-port-%s must carry options:arp_proxy", subX)
	rtrLSP := "rtr-port-" + subX // mirrors topology.SubnetSwitchRouterPort
	lspOpts := harness.OvnNbctl(t, "--no-leader-only", "--bare", "--columns=options",
		"find", "logical_switch_port", "name="+rtrLSP)
	require.Containsf(t, lspOpts, "arp_proxy=169.254.169.254",
		"subnet router LSP %s must set options:arp_proxy=169.254.169.254 so the subnet LRP "+
			"answers ARP for IMDS link-local (static-IP and NetworkManager/RHEL/Ubuntu guests)", rtrLSP)

	// --- IMDSv2 token issuance ----------------------------------------------
	harness.Step(t, "PUT /latest/api/token (IMDSv2)")
	tokenX := imdsAwaitToken(t, fix, tgtX, vpcX, privX)
	harness.Detail(t, "token_len", len(tokenX))

	// --- v2-only stance: tokenless + garbage-token GET → 401 -----------------
	harness.Step(t, "tokenless GET /latest/meta-data/instance-id → 401")
	require.Equal(t, "401", imdsCode(tgtX, "", "/latest/meta-data/instance-id"),
		"IMDSv1 (tokenless) GET must be rejected with 401")
	harness.Step(t, "garbage-token GET → 401")
	require.Equal(t, "401",
		imdsCode(tgtX, `-H "X-aws-ec2-metadata-token: not-a-real-token"`, "/latest/meta-data/instance-id"),
		"unknown token must be rejected with 401 (no token-exists leak)")

	// --- Metadata surface ----------------------------------------------------
	harness.Step(t, "GET metadata surface")
	require.Equal(t, idX, imdsGet(t, tgtX, tokenX, "/latest/meta-data/instance-id"),
		"instance-id mismatch")
	require.Equal(t, privX, imdsGet(t, tgtX, tokenX, "/latest/meta-data/local-ipv4"),
		"local-ipv4 must equal the request source IP")
	require.NotEmpty(t, imdsGet(t, tgtX, tokenX, "/latest/meta-data/instance-type"),
		"instance-type must be populated")
	require.NotEmpty(t, imdsGet(t, tgtX, tokenX, "/latest/meta-data/mac"),
		"mac must be populated")
	require.NotEmpty(t, imdsGet(t, tgtX, tokenX, "/latest/meta-data/placement/availability-zone"),
		"placement/availability-zone must be populated")

	// iam/info → InstanceProfileArn ends with the bound profile.
	iamInfo := imdsGet(t, tgtX, tokenX, "/latest/meta-data/iam/info")
	require.Containsf(t, iamInfo, ":instance-profile/"+imdsProfileName,
		"iam/info must carry the bound InstanceProfileArn (got %q)", iamInfo)
	// security-credentials/ lists the role name (not the profile name).
	require.Equal(t, imdsRoleName, imdsGet(t, tgtX, tokenX, "/latest/meta-data/iam/security-credentials/"),
		"security-credentials/ must list the resolved role name")

	// --- user-data round-trip ------------------------------------------------
	harness.Step(t, "GET /latest/user-data round-trips launch user-data")
	require.Contains(t, imdsGet(t, tgtX, tokenX, "/latest/user-data"), imdsUDMarker,
		"user-data must round-trip the launch user-data")

	// --- Instance-role credentials + wire round-trip -------------------------
	harness.Step(t, "GET security-credentials/%s → ASIA creds", imdsRoleName)
	credBody := imdsGet(t, tgtX, tokenX, "/latest/meta-data/iam/security-credentials/"+imdsRoleName)
	var cred imdsCredDoc
	require.NoError(t, json.Unmarshal([]byte(credBody), &cred),
		"credential body must be valid JSON: %s", credBody)
	require.Equal(t, "Success", cred.Code, "credential Code must be Success (body=%s)", credBody)
	require.True(t, strings.HasPrefix(cred.AccessKeyId, "ASIA"),
		"instance-role AKID must start with ASIA, got %q", cred.AccessKeyId)
	require.NotEmpty(t, cred.SecretAccessKey, "empty SecretAccessKey")
	require.NotEmpty(t, cred.Token, "empty session Token")
	require.Equal(t, adminAccount, cred.AccountId, "credential AccountId mismatch")
	harness.Detail(t, "cred_akid", cred.AccessKeyId)

	// Prove the minted creds verify on the gateway and resolve to the instance-
	// bound assumed-role identity. The session name is the instance ID, so the
	// ARN is .../assumed-role/<role>/<instance-id>.
	harness.Step(t, "get-caller-identity with the IMDS-minted creds (ASIA path)")
	sessionCli := harness.NewAWSClientWithSessionCreds(t, fix.Env,
		cred.AccessKeyId, cred.SecretAccessKey, cred.Token)
	who, err := sessionCli.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "get-caller-identity with instance-role creds")
	expectedARN := "arn:aws:sts::" + adminAccount + ":assumed-role/" + imdsRoleName + "/" + idX
	require.Equal(t, expectedARN, aws.StringValue(who.Arn),
		"IMDS-minted creds must resolve to the instance assumed-role ARN")

	// --- Cross-VPC isolation -------------------------------------------------
	// VM X and VM Y query 169.254.169.254 from the IDENTICAL source IP, in
	// different VPCs. Each must get its own instance-id; a leak here means the
	// per-VPC SO_BINDTODEVICE veth / (VPC-ID, source-IP) → ENI mapping is broken.
	harness.Step(t, "cross-VPC isolation: shared IP %s, distinct identities", privX)
	tokenY := imdsAwaitToken(t, fix, tgtY, vpcY, privY)
	gotY := imdsGet(t, tgtY, tokenY, "/latest/meta-data/instance-id")
	require.Equalf(t, idY, gotY, "VM Y IMDS must return VM Y's instance-id (got %q)", gotY)
	require.NotEqualf(t, idX, gotY,
		"cross-VPC leak: VM Y (source IP %s) resolved to VM X — the "+
			"(VPC-ID, source-IP) → ENI boundary is broken", privX)
	// And VM X still resolves to itself (not Y) after Y came up on the same IP.
	require.Equal(t, idX, imdsGet(t, tgtX, tokenX, "/latest/meta-data/instance-id"),
		"VM X IMDS must still return VM X's instance-id")
	harness.Detail(t, "isolation", "ok")
}

// imdsVMSpec parameterises a launch for the IMDS suite. profileName / userData
// are optional.
type imdsVMSpec struct {
	subnetID    string
	sgID        string
	profileName string
	userData    string
}

// imdsProbe launches a VM per spec, waits for it to run + become SSH-reachable,
// and returns (instanceID, privateIP, sshTarget). Registers terminate cleanup
// so the VM is torn down before its VPC/subnet are deleted (LIFO).
func imdsProbe(t *testing.T, fix *Fixture, keyPath string, spec imdsVMSpec) (string, string, harness.SSHTarget) {
	t.Helper()
	id := imdsRunVM(t, fix, spec)
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		_ = waitForInstanceStateSoft(fix.AWS, id, "terminated", 5*time.Minute)
	})

	inst := harness.WaitForInstanceState(t, fix.AWS, id, "running")
	priv := aws.StringValue(inst.PrivateIpAddress)
	require.NotEmptyf(t, priv, "instance %s has no PrivateIpAddress", id)

	host, port := harness.InstancePublicSSHHost(t, inst)
	harness.Step(t, "wait for %s SSH at %s:%d", id, host, port)
	waitForSSHHandshake(t, host, port, keyPath)
	return id, priv, harness.SSHTarget{User: "ec2-user", Host: host, Port: port, KeyPath: keyPath}
}

// imdsRunVM launches a single instance per spec and returns its ID. AMI / type /
// key come from the suite discovery helpers.
func imdsRunVM(t *testing.T, fix *Fixture, spec imdsVMSpec) string {
	t.Helper()
	amiID := needAMI(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, _ := needKeyPair(t, fix)

	in := &ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(spec.subnetID),
		SecurityGroupIds: []*string{aws.String(spec.sgID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	}
	if spec.profileName != "" {
		in.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{Name: aws.String(spec.profileName)}
	}
	if spec.userData != "" {
		in.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(spec.userData)))
	}
	out, err := fix.AWS.EC2.RunInstances(in)
	require.NoError(t, err, "run-instances subnet=%s", spec.subnetID)
	require.NotEmpty(t, out.Instances, "run-instances returned no Instances")
	id := aws.StringValue(out.Instances[0].InstanceId)
	require.True(t, strings.HasPrefix(id, "i-"), "unexpected InstanceId %q", id)
	return id
}

// imdsEnsureProbeVPC creates a VPC + subnet wired for SSH reachability and
// returns (vpcID, subnetID, sgID). In pool mode it adds the public-IP path
// (MapPublicIpOnLaunch + IGW + 0.0.0.0/0 route) so the runner reaches the guest
// by public IP; in dev_networking mode SSH lands via the per-VM qemu hostfwd, so
// the IGW plumbing is skipped. All resources are torn down LIFO via t.Cleanup.
func imdsEnsureProbeVPC(t *testing.T, fix *Fixture, vpcCIDR, subnetCIDR string) (string, string, string) {
	t.Helper()
	c := fix.AWS

	vpcOut, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(vpcCIDR)})
	require.NoError(t, err, "create-vpc %s", vpcCIDR)
	vpcID := aws.StringValue(vpcOut.Vpc.VpcId)
	require.NotEmpty(t, vpcID, "VpcId empty")
	t.Cleanup(func() { _, _ = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}) })

	subOut, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String(subnetCIDR),
	})
	require.NoError(t, err, "create-subnet %s", subnetCIDR)
	subnetID := aws.StringValue(subOut.Subnet.SubnetId)
	require.NotEmpty(t, subnetID, "SubnetId empty")
	t.Cleanup(func() { _, _ = c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}) })

	if fix.PoolMode {
		imdsAttachInternet(t, c, vpcID, subnetID)
	}

	sgID := imdsDefaultSG(t, c, vpcID)
	harness.AuthorizeSSHIngress(t, c, sgID)
	harness.Detail(t, "probe_vpc", vpcID, "subnet", subnetID, "sg", sgID)
	return vpcID, subnetID, sgID
}

// imdsAttachInternet gives subnetID a public-IP egress path (pool mode only):
// MapPublicIpOnLaunch + a fresh IGW + a 0.0.0.0/0 route table. Cleanups unwind
// LIFO before the subnet/VPC are deleted by the caller's earlier-registered
// cleanups.
func imdsAttachInternet(t *testing.T, c *harness.AWSClient, vpcID, subnetID string) {
	t.Helper()
	_, err := c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(subnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "modify-subnet-attribute MapPublicIpOnLaunch")

	igwOut, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	igwID := aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
	require.NotEmpty(t, igwID, "InternetGatewayId empty")
	t.Cleanup(func() {
		_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
		})
		_, _ = c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String(igwID)})
	})
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway")

	rtOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(vpcID)})
	require.NoError(t, err, "create-route-table")
	rtbID := aws.StringValue(rtOut.RouteTable.RouteTableId)
	require.NotEmpty(t, rtbID, "RouteTableId empty")
	t.Cleanup(func() { _, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String(rtbID)}) })

	assocOut, err := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID), SubnetId: aws.String(subnetID),
	})
	require.NoError(t, err, "associate-route-table")
	assocID := aws.StringValue(assocOut.AssociationId)
	t.Cleanup(func() {
		_, _ = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{AssociationId: aws.String(assocID)})
	})

	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(igwID),
	})
	require.NoError(t, err, "create-route 0.0.0.0/0 -> IGW")
}

// imdsEnsureRoleProfile creates the EC2-trusted role + instance profile the
// IMDS credential path resolves, attaching AdministratorAccess so the minted
// session creds round-trip a real call. Registers fixture-teardown cleanup and
// sweeps any residue from a prior run first.
func imdsEnsureRoleProfile(t *testing.T, fix *Fixture, adminAccount string) {
	t.Helper()
	adminPolicyARN := iamPolicyARN(adminAccount, iamPolicyAdministrator)
	iamDeleteRoleAndProfilesBestEffort(fix, imdsRoleName, []string{imdsProfileName}, adminPolicyARN)
	fix.Harness.RegisterCleanup(func() {
		iamDeleteRoleAndProfilesBestEffort(fix, imdsRoleName, []string{imdsProfileName}, adminPolicyARN)
	})

	harness.Step(t, "create-role %q (trust=ec2.amazonaws.com) + profile %q", imdsRoleName, imdsProfileName)
	_, err := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(imdsRoleName),
		AssumeRolePolicyDocument: aws.String(iamTrustPolicyEC2Standard),
		Description:              aws.String("E2E IMDS instance-role credentials"),
	})
	require.NoError(t, err, "create-role")

	_, err = fix.AWS.IAM.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  aws.String(imdsRoleName),
		PolicyArn: aws.String(adminPolicyARN),
	})
	require.NoError(t, err, "attach AdministratorAccess to role")

	_, err = fix.AWS.IAM.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(imdsProfileName),
	})
	require.NoError(t, err, "create-instance-profile")
	_, err = fix.AWS.IAM.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(imdsProfileName),
		RoleName:            aws.String(imdsRoleName),
	})
	require.NoError(t, err, "add-role-to-instance-profile")
}

// imdsAwaitToken PUTs an IMDSv2 token from inside the guest, retrying to ride
// out the cold-start window before BindManager.Sync + the ENI reverse-index
// land. Returns the token. On timeout it dumps the per-VPC IMDS datapath
// (OVN realisation + host veth route/neigh + conntrack) before failing, so a
// reachability timeout (exit 28) is triaged as request-path vs reply-path
// rather than a bare "condition not met".
func imdsAwaitToken(t *testing.T, fix *Fixture, tgt harness.SSHTarget, vpcID, guestIP string) string {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for {
		cmd := fmt.Sprintf(
			`curl -sf -X PUT "%s/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 120"`, metaURL)
		out, err := runSSHCombined(tgt, cmd)
		switch {
		case err != nil:
			lastErr = fmt.Errorf("token PUT failed: %w (out=%q)", err, out)
		case strings.TrimSpace(out) == "":
			lastErr = fmt.Errorf("token PUT returned empty body")
		default:
			return strings.TrimSpace(out)
		}
		if time.Now().After(deadline) {
			harness.DumpIMDSDatapathDiagnostics(t, vpcID, guestIP, fix.ArtifactDir(t))
			t.Fatalf("imdsAwaitToken: token never minted within 90s (vpc=%s guest=%s): %v "+
				"(see IMDS datapath diagnostics above)", vpcID, guestIP, lastErr)
		}
		time.Sleep(3 * time.Second)
	}
}

// imdsGet fetches a token-gated path from inside the guest and returns the
// trimmed body. -sf turns any HTTP >= 400 into a non-zero exit, so a 4xx/5xx
// fails the test loudly with the server status visible via runSSH's stderr.
func imdsGet(t *testing.T, tgt harness.SSHTarget, token, path string) string {
	t.Helper()
	cmd := fmt.Sprintf(`curl -sf -H "X-aws-ec2-metadata-token: %s" %s%s`, token, metaURL, path)
	return strings.TrimSpace(runSSH(t, tgt, cmd))
}

// imdsCode returns the HTTP status code (as a string) the guest sees for a GET,
// without failing on non-2xx. headerArg is an optional pre-formatted curl -H
// argument (empty for the tokenless probe).
func imdsCode(tgt harness.SSHTarget, headerArg, path string) string {
	cmd := fmt.Sprintf(`curl -s -o /dev/null -w '%%{http_code}' %s %s%s`, headerArg, metaURL, path)
	out, _ := runSSHCombined(tgt, cmd)
	return strings.TrimSpace(out)
}

// imdsDefaultSG returns the auto-created default security group ID for a VPC.
func imdsDefaultSG(t *testing.T, c *harness.AWSClient, vpcID string) string {
	t.Helper()
	out, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
			{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
		},
	})
	require.NoError(t, err, "describe-security-groups default (vpc=%s)", vpcID)
	require.NotEmpty(t, out.SecurityGroups, "no default SG for vpc=%s", vpcID)
	id := aws.StringValue(out.SecurityGroups[0].GroupId)
	require.NotEmpty(t, id, "default SG GroupId empty (vpc=%s)", vpcID)
	return id
}
