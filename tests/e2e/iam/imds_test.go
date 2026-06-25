//go:build e2e

package iam

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// IMDS E2E identifiers. The role/profile carry an imds-e2e- prefix so they
// never collide with the IAM-roles suite ("app-role"/"app-profile") or the
// STS suite ("sts-e2e-role").
const (
	imdsRoleName    = "imds-e2e-role"
	imdsProfileName = "imds-e2e-profile"

	// imdsAdminPolicyName is the bootstrap managed policy attached to the IMDS
	// role so the minted instance-role creds round-trip a real API call.
	imdsAdminPolicyName = "AdministratorAccess"
	// imdsTrustPolicyEC2 lets ec2.amazonaws.com assume the IMDS instance role.
	imdsTrustPolicyEC2 = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

	// metaURL is the AWS-compatible link-local IMDS endpoint, answered at the
	// guest's own host tap by the per-tap responder: the tap rides the OVN-
	// unmanaged br-imds bridge, where a demux flow steers 169.254.169.254 to a
	// SO_BINDTODEVICE-bound responder and a patch carries everything else to
	// br-int. Identity is the tap (→ ENI), not the source IP; no per-subnet
	// localport, no netns, no in-guest route.
	metaURL = "http://169.254.169.254"

	// imdsUserData is a #cloud-config carrying a unique marker, used two ways: the
	// /latest/user-data round-trip asserts the body the guest sees matches what
	// RunInstances was given, and its runcmd writes imdsUDMarker to imdsUDDoneFile
	// so the boot-from-IMDS guard can prove cloud-init actually RAN user-data from
	// the Ec2 datasource, not just that the responder served it.
	imdsUserData = "#cloud-config\n" +
		"# imds-e2e-userdata-marker-7f3a91\n" +
		"runcmd:\n" +
		"  - [ sh, -c, \"echo imds-e2e-userdata-marker-7f3a91 > /run/imds-e2e-userdata.done\" ]\n"
	imdsUDMarker   = "imds-e2e-userdata-marker-7f3a91"
	imdsUDDoneFile = "/run/imds-e2e-userdata.done"

	// imdsProbeVPCCIDR / imdsProbeSubnetCIDR are shared by BOTH isolation VPCs.
	// Overlapping CIDRs across VPCs are legal (each VPC is its own routing
	// domain); using identical subnets makes IPAM hand the first VM in each the
	// SAME private IP (network+4, deterministic per ipam.go). With a shared
	// source IP the only thing that can tell the two VMs apart is their tap, so
	// this is exactly the case the per-tap tap→ENI identity must resolve.
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
// VMs over the per-tap datapath. It stands up two VPCs with identical subnet
// CIDRs and one VM in each so the two VMs share a private IP:
//
//   - VM X (profile-bound, user-data) drives the full surface: IMDSv2 token
//     issuance, the v2-only stance (tokenless / garbage-token GET → 401), the
//     metadata fields, the instance-role credential path + a wire round-trip
//     proving the ASIA creds resolve to the instance assumed-role ARN, and the
//     /latest/user-data round-trip — all reached from the guest with no in-guest
//     route, since the responder intercepts at the tap regardless of addressing.
//   - The per-tap datapath shape: the guest's tap + the ime-/imp- endpoint and
//     patch live on the OVN-unmanaged br-imds bridge, the imi- patch end on
//     br-int carries the guest's OVN iface-id, and br-imds carries the demux
//     (priority=200, capturing 169.254.169.254) and forward (priority=100) flow
//     tiers. Host-local; skipped when the VM landed on another chassis.
//   - The file-capability proof: the live vpcd serves under the restored sandbox
//     with CAP_NET_ADMIN/NET_RAW/NET_BIND_SERVICE and no CAP_SYS_ADMIN.
//   - Cross-VPC isolation: VM X and VM Y share the same source IP in different
//     VPCs, yet each IMDS returns its OWN instance-id — the tap→ENI identity
//     boundary, which a source-IP lookup could not disambiguate.
//
// OVN + SSH gated: the IMDS datapath needs ovn-controller to bind the guest LSP
// to the patch per chassis, and every guest-facing assertion runs via SSH.
func runIMDS(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — IMDSv2 Host-Served Instance Metadata")
	harness.SkipIfNoOVN(t)
	requireSSHHealthy(t)

	adminAccount := harness.IAMAccountID(t, fix.AWS)
	keyName, keyPath := needKeyPair(t, fix)
	imdsEnsureRoleProfile(t, fix, adminAccount)

	// Two VPCs, identical subnet CIDR → the first VM in each gets the same IP.
	// The VPC IDs are logged inside imdsEnsureProbeVPC; the L2 datapath and the
	// isolation assertions key off the subnet and the instance identity, not the
	// VPC ID, so they aren't bound here.
	_, subX, sgX := imdsEnsureProbeVPC(t, fix, imdsProbeVPCCIDR, imdsProbeSubnetCIDR)
	_, subY, sgY := imdsEnsureProbeVPC(t, fix, imdsProbeVPCCIDR, imdsProbeSubnetCIDR)

	// VM X — profile-bound + user-data; drives the full surface. eniX builds the
	// guest LSP name (port-<eni>) for the ovn-trace datapath assertion.
	idX, privX, eniX, tgtX := imdsProbe(t, fix, keyPath, imdsVMSpec{
		subnetID:    subX,
		sgID:        sgX,
		profileName: imdsProfileName,
		userData:    imdsUserData,
	})
	// VM Y — isolation peer; no profile needed for the instance-id probe.
	idY, privY, _, tgtY := imdsProbe(t, fix, keyPath, imdsVMSpec{
		subnetID: subY,
		sgID:     sgY,
	})
	require.NotEqual(t, idX, idY, "the two probe VMs must be distinct instances")
	require.Equalf(t, privX, privY,
		"isolation premise: both first-in-subnet VMs must share a private IP "+
			"(IPAM is deterministic at network+4) — got %s vs %s", privX, privY)
	harness.Detail(t, "vm_x", idX, "vm_y", idY, "shared_priv", privX)

	// --- Per-tap datapath shape (host-local) + file-capability proof ---------
	// The per-tap successor to the retired per-subnet ovn-trace L2 assertion:
	// the guest's tap rides br-imds with the endpoint/patch ports and flow tiers,
	// and the live vpcd serves it without CAP_SYS_ADMIN.
	imdsAssertPerTapDatapath(t, eniX)
	imdsAssertVpcdFileCaps(t)

	// --- IMDSv2 token issuance ----------------------------------------------
	harness.Step(t, "PUT /latest/api/token (IMDSv2)")
	tokenX := imdsAwaitToken(t, fix, tgtX, subX, privX)
	harness.Detail(t, "token_len", len(tokenX))

	// --- v2-only stance: tokenless + garbage-token GET → 401 -----------------
	harness.Step(t, "tokenless GET /latest/meta-data/instance-id → 401")
	require.Equal(t, "401", imdsCode(tgtX, "", "/latest/meta-data/instance-id"),
		"IMDSv1 (tokenless) GET must be rejected with 401")
	harness.Step(t, "garbage-token GET → 401")
	require.Equal(t, "401",
		imdsCode(tgtX, `-H "X-aws-ec2-metadata-token: not-a-real-token"`, "/latest/meta-data/instance-id"),
		"unknown token must be rejected with 401 (no token-exists leak)")

	// --- SSRF defence: X-Forwarded-For → 403 even with a valid token ---------
	// A forwarded request is a proxied SSRF attempt; the host refuses it before
	// the token is consulted, so a valid token does not rescue it.
	harness.Step(t, "X-Forwarded-For GET with valid token → 403")
	require.Equal(t, "403",
		imdsCode(tgtX, fmt.Sprintf(`-H "X-aws-ec2-metadata-token: %s" -H "X-Forwarded-For: 10.0.0.1"`, tokenX),
			"/latest/meta-data/instance-id"),
		"X-Forwarded-For must be rejected with 403 even with a valid token (SSRF defence)")

	// --- Token-TTL validation: missing / 0 / over-max / non-numeric → 400 ----
	// PUT /latest/api/token requires X-aws-ec2-metadata-token-ttl-seconds in
	// [1,21600]; anything outside that mints no token and returns 400.
	for _, tc := range []struct{ label, header string }{
		{"missing", `-X PUT`},
		{"zero", `-X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: 0"`},
		{"over-max", `-X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: 21601"`},
		{"non-numeric", `-X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: abc"`},
	} {
		harness.Step(t, "PUT token with %s TTL → 400", tc.label)
		require.Equalf(t, "400", imdsCode(tgtX, tc.header, "/latest/api/token"),
			"PUT token with %s TTL must be 400", tc.label)
	}

	// --- Method-not-allowed: token is PUT-only, metadata is GET-only → 405 ---
	// The method check precedes token validation, so neither probe needs a token.
	harness.Step(t, "GET /latest/api/token → 405; PUT metadata path → 405")
	require.Equal(t, "405", imdsCode(tgtX, "", "/latest/api/token"),
		"GET on the PUT-only token endpoint must be 405")
	require.Equal(t, "405", imdsCode(tgtX, `-X PUT`, "/latest/meta-data/instance-id"),
		"PUT on a GET-only metadata path must be 405")

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

	// Cheap one-field/static leaves: a real reservation id, the static
	// instance-life-cycle, and the services subtree.
	require.True(t, strings.HasPrefix(imdsGet(t, tgtX, tokenX, "/latest/meta-data/reservation-id"), "r-"),
		"reservation-id must be served as an r-… identifier")
	require.Equal(t, "on-demand", imdsGet(t, tgtX, tokenX, "/latest/meta-data/instance-life-cycle"),
		"instance-life-cycle must be on-demand (Spot not modelled)")
	require.Equal(t, "aws", imdsGet(t, tgtX, tokenX, "/latest/meta-data/services/partition"),
		"services/partition must be aws")
	require.Equal(t, "amazonaws.com", imdsGet(t, tgtX, tokenX, "/latest/meta-data/services/domain"),
		"services/domain must be amazonaws.com")

	// public-hostname mirrors public-ipv4 when the instance has one, else 404. In
	// pool mode the probe subnet maps a public IP on launch; in dev_networking it
	// has none, so the two modes exercise the two branches.
	if fix.PoolMode {
		pubIP := imdsGet(t, tgtX, tokenX, "/latest/meta-data/public-ipv4")
		require.NotEmpty(t, pubIP, "pool-mode VM must have a public IP")
		require.Equal(t, pubIP, imdsGet(t, tgtX, tokenX, "/latest/meta-data/public-hostname"),
			"public-hostname must mirror public-ipv4")
	} else {
		require.Equal(t, "404",
			imdsCode(tgtX, fmt.Sprintf(`-H "X-aws-ec2-metadata-token: %s"`, tokenX),
				"/latest/meta-data/public-hostname"),
			"no public IP → public-hostname must 404")
	}

	// VM X is profile-bound, so meta-data/ lists iam/ and cloud-init descends.
	require.Contains(t, imdsGet(t, tgtX, tokenX, "/latest/meta-data/"), "iam/",
		"profile-bound meta-data/ listing must include iam/")
	// iam/info → InstanceProfileArn ends with the bound profile.
	iamInfo := imdsGet(t, tgtX, tokenX, "/latest/meta-data/iam/info")
	require.Containsf(t, iamInfo, ":instance-profile/"+imdsProfileName,
		"iam/info must carry the bound InstanceProfileArn (got %q)", iamInfo)
	// security-credentials/ lists the role name (not the profile name).
	require.Equal(t, imdsRoleName, imdsGet(t, tgtX, tokenX, "/latest/meta-data/iam/security-credentials/"),
		"security-credentials/ must list the resolved role name")

	// --- public-keys ---------------------------------------------------------
	// The launch key injection path cloud-init's Ec2 datasource queries: the two
	// directory listings plus the material leaf, served live from the key store.
	harness.Step(t, "GET /latest/meta-data/public-keys/ subtree")
	require.Equal(t, "0="+keyName, imdsGet(t, tgtX, tokenX, "/latest/meta-data/public-keys/"),
		"public-keys/ must list 0=<keyName>")
	require.Equal(t, "openssh-key", imdsGet(t, tgtX, tokenX, "/latest/meta-data/public-keys/0/"),
		"public-keys/0/ must list the openssh-key format")
	gotKey := imdsGet(t, tgtX, tokenX, "/latest/meta-data/public-keys/0/openssh-key")
	wantType, wantB64 := imdsExpectedPubKey(t, keyPath)
	gotFields := strings.Fields(gotKey)
	require.GreaterOrEqualf(t, len(gotFields), 2,
		"public-keys/0/openssh-key must serve <type> <base64> (got %q)", gotKey)
	require.Equal(t, wantType, gotFields[0], "public key type mismatch")
	require.Equal(t, wantB64, gotFields[1],
		"public-keys/0/openssh-key material must match the launch key pair")

	// --- user-data round-trip ------------------------------------------------
	harness.Step(t, "GET /latest/user-data round-trips launch user-data")
	require.Contains(t, imdsGet(t, tgtX, tokenX, "/latest/user-data"), imdsUDMarker,
		"user-data must round-trip the launch user-data")

	// --- Boot-from-IMDS cutover guards ---------------------------------------
	// The rest of this suite proves the guest can READ IMDS; this proves it BOOTED
	// from it. The NoCloud seed is retired, so every guest now self-configures from
	// the Ec2 datasource — and every other assertion here still passes if the seed
	// silently comes back, so this is the only guard against that regression.
	imdsAssertBootFromIMDS(t, tgtX, privX)

	// --- Version discovery + dated-version alias (cloud-init parity) ----------
	// cloud-init's EC2 datasource probes its OWN hardcoded dated versions
	// (e.g. 2021-03-23), not the GET / listing, so any dated prefix must alias to
	// /latest. Prove that aliasing resolves in-guest.
	harness.Step(t, "version discovery: GET /, GET /latest, dated-version alias")
	require.Contains(t, imdsGet(t, tgtX, tokenX, "/"), "latest",
		"GET / must advertise the supported version list")
	require.Equal(t, "dynamic\nmeta-data\nuser-data", imdsGet(t, tgtX, tokenX, "/latest"),
		"GET /latest must list the top-level tree")
	require.Equal(t, idX, imdsGet(t, tgtX, tokenX, "/2021-03-23/meta-data/instance-id"),
		"a dated API version must alias to /latest (cloud-init parity)")

	// --- Dynamic instance-identity document ----------------------------------
	// The unsigned document is consumed by real SDKs in-guest; assert the fields
	// resolved from ENI + instance facts, including the launch-time architecture.
	harness.Step(t, "GET /latest/dynamic/instance-identity/document")
	_, wantArch := needInstanceTypeArch(t, fix)
	docBody := imdsGet(t, tgtX, tokenX, "/latest/dynamic/instance-identity/document")
	var idDoc struct {
		InstanceID   string `json:"instanceId"`
		AccountID    string `json:"accountId"`
		Region       string `json:"region"`
		Architecture string `json:"architecture"`
	}
	require.NoError(t, json.Unmarshal([]byte(docBody), &idDoc),
		"identity document must be valid JSON: %s", docBody)
	require.Equal(t, idX, idDoc.InstanceID, "identity document instanceId mismatch")
	require.Equal(t, adminAccount, idDoc.AccountID, "identity document accountId mismatch")
	require.NotEmpty(t, idDoc.Region, "identity document region must be populated")
	require.Equal(t, wantArch, idDoc.Architecture,
		"identity document architecture must match the launch instance type")

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
	// different VPCs (hence different subnets). Each must get its own instance-id;
	// a leak here means the responder consulted the source IP instead of the tap —
	// the tap→ENI identity boundary is broken.
	harness.Step(t, "cross-VPC isolation: shared IP %s, distinct identities", privX)
	tokenY := imdsAwaitToken(t, fix, tgtY, subY, privY)
	gotY := imdsGet(t, tgtY, tokenY, "/latest/meta-data/instance-id")
	require.Equalf(t, idY, gotY, "VM Y IMDS must return VM Y's instance-id (got %q)", gotY)
	require.NotEqualf(t, idX, gotY,
		"cross-VPC leak: VM Y (source IP %s) resolved to VM X — the tap→ENI "+
			"identity boundary is broken (source IP is not identity)", privX)
	// And VM X still resolves to itself (not Y) after Y came up on the same IP.
	require.Equal(t, idX, imdsGet(t, tgtX, tokenX, "/latest/meta-data/instance-id"),
		"VM X IMDS must still return VM X's instance-id")
	harness.Detail(t, "isolation", "ok")

	// --- Cross-instance token binding + no-profile surface -------------------
	// Re-mint a fresh VM X token so the binding rejection below is unambiguously
	// about ENI binding rather than TTL expiry of the token minted early above.
	freshTokenX := imdsAwaitToken(t, fix, tgtX, subX, privX)

	harness.Step(t, "VM Y presents VM X's token → 401 (ENI-bound)")
	require.Equal(t, "401",
		imdsCode(tgtY, fmt.Sprintf(`-H "X-aws-ec2-metadata-token: %s"`, freshTokenX),
			"/latest/meta-data/instance-id"),
		"a token bound to VM X's ENI must not authorise VM Y")

	// VM Y has no instance profile, so the whole iam/ subtree is absent: real EC2
	// omits iam/ from the meta-data/ listing and 404s the iam/ directory, so
	// cloud-init never descends and never trips on a 404ing iam/info that would fail
	// its metadata crawl and zombie the guest.
	harness.Step(t, "VM Y meta-data/ omits iam/; iam/ directory 404s")
	require.NotContains(t, imdsGet(t, tgtY, tokenY, "/latest/meta-data/"), "iam/",
		"no-profile meta-data/ listing must omit iam/")
	for _, p := range []string{"/latest/meta-data/iam", "/latest/meta-data/iam/"} {
		require.Equalf(t, "404",
			imdsCode(tgtY, fmt.Sprintf(`-H "X-aws-ec2-metadata-token: %s"`, tokenY), p),
			"no-profile %s must 404 (real-EC2 parity)", p)
	}

	// VM Y has no instance profile: iam/info is 404 and the credential listing is
	// an empty 200 (absence is not an error, matching AWS).
	harness.Step(t, "VM Y iam/info → 404; security-credentials/ → empty 200")
	require.Equal(t, "404",
		imdsCode(tgtY, fmt.Sprintf(`-H "X-aws-ec2-metadata-token: %s"`, tokenY),
			"/latest/meta-data/iam/info"),
		"an instance with no profile must 404 on iam/info")
	require.Equal(t, "200",
		imdsCode(tgtY, fmt.Sprintf(`-H "X-aws-ec2-metadata-token: %s"`, tokenY),
			"/latest/meta-data/iam/security-credentials/"),
		"no-profile security-credentials/ listing must be an empty 200")
	require.Empty(t, imdsGet(t, tgtY, tokenY, "/latest/meta-data/iam/security-credentials/"),
		"no-profile security-credentials/ body must be empty")

	// VM X is profile-bound: a credential request for a role it isn't bound to
	// is a 404, not a leak of the bound role's creds.
	harness.Step(t, "VM X credential request for an unbound role name → 404")
	require.Equal(t, "404",
		imdsCode(tgtX, fmt.Sprintf(`-H "X-aws-ec2-metadata-token: %s"`, freshTokenX),
			"/latest/meta-data/iam/security-credentials/wrong-role-name"),
		"a credential request for a role the instance isn't bound to must 404")
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
// and returns (instanceID, privateIP, eniID, sshTarget). eniID drives the per-tap
// datapath shape assertion (its tap/endpoint/patch port names). Registers
// terminate cleanup so the VM is torn down before its VPC/subnet are deleted (LIFO).
func imdsProbe(t *testing.T, fix *Fixture, keyPath string, spec imdsVMSpec) (string, string, string, harness.SSHTarget) {
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
	eni := primaryENI(t, inst)

	host, port := harness.InstancePublicSSHHost(t, inst)
	harness.Step(t, "wait for %s SSH at %s:%d", id, host, port)
	waitForSSHHandshake(t, host, port, keyPath)
	return id, priv, eni, harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
}

// imdsAssertPerTapDatapath asserts the per-tap IMDS datapath is realised on the
// LOCAL chassis for the guest's ENI. It is the per-tap successor to the retired
// per-subnet ovn-trace L2 assertion and guards the steering bug the runtime smoke
// caught: the primary tap must be a port on the OVN-unmanaged br-imds (not br-int)
// so the demux flows meet its egress. Checks:
//
//   - the tap + the ime-/imp- endpoint and patch ports are on br-imds;
//   - the imi- patch end on br-int carries the guest's OVN iface-id, so
//     ovn-controller binds the guest LSP to the patch exactly as to the tap;
//   - br-imds carries the demux (priority=200, capturing 169.254.169.254) and the
//     transparent forward (priority=100) flow tiers.
//
// br-imds ports are per-chassis OVS state, so this is host-local: it skips when
// the guest landed on another chassis (multinode) or the runner is remote.
func imdsAssertPerTapDatapath(t *testing.T, eniID string) {
	t.Helper()
	short := imdsShortENI(eniID)
	tap := imdsTapName(eniID)
	endpoint := "ime-" + short  // host.IMDSEndpointName
	patchIMDS := "imp-" + short // host.IMDSPatchPort (br-imds end)
	patchInt := "imi-" + short  // host.IMDSIntPatchPort (br-int end)

	harness.Step(t, "per-tap datapath: tap %s + endpoint/patch on br-imds", tap)
	brimdsPorts := harness.OvsVsctl(t, "list-ports", "br-imds")
	if !strings.Contains(brimdsPorts, tap) {
		t.Skipf("guest tap %s not on local br-imds (ports=%q) — VM on another chassis "+
			"or remote runner; per-tap datapath shape is host-local", tap, brimdsPorts)
	}
	require.Containsf(t, brimdsPorts, endpoint,
		"per-tap endpoint %s must be a br-imds port (the SO_BINDTODEVICE target the responder binds)", endpoint)
	require.Containsf(t, brimdsPorts, patchIMDS,
		"per-tap patch %s must be a br-imds port (the transparent hop to br-int)", patchIMDS)

	// The br-int patch end carries the OVN iface-id so ovn-controller binds the
	// guest LSP to it exactly as it bound the tap before the move to br-imds.
	wantIface := "port-" + eniID // vm.OVSIfaceID == topology.Port
	gotIface := harness.OvsVsctl(t, "--bare", "--columns=external_ids",
		"find", "Interface", "name="+patchInt)
	require.Containsf(t, gotIface, "iface-id="+wantIface,
		"br-int patch end %s must carry external_ids:iface-id=%s so ovn-controller binds the "+
			"guest LSP to the patch (got %q)", patchInt, wantIface, gotIface)

	// Both flow tiers must be present: the demux captures 169.254.169.254 at
	// priority 200, the forward bridges everything else to br-int at 100.
	harness.Step(t, "per-tap datapath: br-imds demux + forward flow tiers")
	flows := harness.OvsOfctl(t, "dump-flows", "br-imds")
	require.Containsf(t, flows, "priority=200",
		"br-imds must carry the demux flow tier (priority=200); flows:\n%s", flows)
	require.Containsf(t, flows, "nw_dst=169.254.169.254",
		"br-imds demux must capture 169.254.169.254; flows:\n%s", flows)
	require.Containsf(t, flows, "priority=100",
		"br-imds must carry the transparent forward flow tier (priority=100) so non-IMDS "+
			"traffic still reaches br-int; flows:\n%s", flows)
}

// imdsAssertVpcdFileCaps proves the per-tap cutover's capability claim against the
// live daemon: vpcd serves IMDS under the restored sandbox with CAP_NET_ADMIN /
// CAP_NET_RAW / CAP_NET_BIND_SERVICE and WITHOUT CAP_SYS_ADMIN — the setns/netns
// path that required CAP_SYS_ADMIN is gone. Reads the live vpcd CapEff; skips
// where vpcd is not local.
func imdsAssertVpcdFileCaps(t *testing.T) {
	t.Helper()
	// Capability bit positions (linux/capability.h).
	const (
		capNetBindService = 10
		capNetAdmin       = 12
		capNetRaw         = 13
		capSysAdmin       = 21
	)
	caps, ok := harness.EffectiveCapsForUnit(t, "spinifex-vpcd")
	if !ok {
		t.Skip("spinifex-vpcd MainPID/CapEff not readable on this host; the file-cap proof " +
			"requires a local vpcd (single-node chassis)")
	}
	harness.Step(t, "vpcd CapEff=0x%x: CAP_SYS_ADMIN dropped, net caps retained", caps)
	require.Zerof(t, caps&(uint64(1)<<capSysAdmin),
		"vpcd must NOT hold CAP_SYS_ADMIN after the per-tap cutover (CapEff=0x%x) — the "+
			"setns/netns path that required it is gone", caps)
	for _, c := range []struct {
		bit  uint
		name string
	}{
		{capNetAdmin, "CAP_NET_ADMIN"},
		{capNetRaw, "CAP_NET_RAW"},
		{capNetBindService, "CAP_NET_BIND_SERVICE"},
	} {
		require.NotZerof(t, caps&(uint64(1)<<c.bit),
			"vpcd must retain %s to serve the per-tap datapath (CapEff=0x%x)", c.name, caps)
	}
}

// imdsAssertBootFromIMDS proves VM X bootstrapped from the Ec2 IMDS datasource
// with the NoCloud seed retired — a regression guard the rest of the suite can't
// give: every other assertion still passes if the seed silently comes back and
// cloud-init boots from NoCloud instead. Checks, all in-guest:
//
//   - cloud-init reports the aws platform / an Ec2 datasource (not NoCloud);
//   - no cidata-labelled block device is attached (the seed ISO is gone);
//   - the login is the AMI's stock default user (no Spinifex-forced account);
//   - the hostname is the AWS form ip-<dashed-ip>;
//   - the primary NIC carries the VPC IP rendered from IMDS;
//   - the user-data runcmd executed (cloud-init processed user-data from IMDS).
func imdsAssertBootFromIMDS(t *testing.T, tgt harness.SSHTarget, privIP string) {
	t.Helper()
	harness.Step(t, "boot-from-IMDS: cloud-init selected the aws (Ec2) datasource")
	ds := strings.ToLower(strings.TrimSpace(
		runSSH(t, tgt, "cloud-id 2>/dev/null || cloud-init query --format '{{datasource}}'")))
	require.Truef(t, strings.Contains(ds, "aws") || strings.Contains(ds, "ec2"),
		"cloud-init must report the aws/Ec2 datasource (got %q) — not the retired NoCloud seed", ds)

	harness.Step(t, "boot-from-IMDS: no NoCloud cidata seed device attached")
	labels := strings.ToLower(runSSH(t, tgt, "lsblk -no LABEL"))
	require.NotContainsf(t, labels, "cidata",
		"no cidata-labelled device may be attached — the seed ISO is retired (lsblk LABELs: %q)", labels)

	harness.Step(t, "boot-from-IMDS: login is the AMI stock default user %q", tgt.User)
	require.Equal(t, tgt.User, strings.TrimSpace(runSSH(t, tgt, "id -un")),
		"in-guest login must be the AMI stock default user, not a Spinifex-forced account")

	harness.Step(t, "boot-from-IMDS: AWS-form hostname ip-<dashed-ip>")
	wantHost := "ip-" + strings.ReplaceAll(privIP, ".", "-")
	gotHost := strings.TrimSpace(runSSH(t, tgt, "hostname"))
	require.Truef(t, strings.HasPrefix(gotHost, wantHost),
		"hostname must be the AWS form %q rendered from IMDS local-hostname (got %q)", wantHost, gotHost)

	harness.Step(t, "boot-from-IMDS: primary NIC up with the VPC IP %s", privIP)
	addrs := runSSH(t, tgt, "ip -4 -o addr show scope global")
	require.Containsf(t, addrs, privIP,
		"the Ec2 datasource must render the primary NIC with the VPC IP %s from IMDS (ip addr: %q)", privIP, addrs)

	harness.Step(t, "boot-from-IMDS: user-data runcmd executed")
	deadline := time.Now().Add(90 * time.Second)
	for {
		out, _ := runSSHCombined(tgt, "cat "+imdsUDDoneFile+" 2>/dev/null")
		if strings.Contains(out, imdsUDMarker) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("user-data runcmd marker %s never appeared within 90s — "+
				"cloud-init did not run user-data from the Ec2 datasource", imdsUDDoneFile)
		}
		time.Sleep(3 * time.Second)
	}
}

// imdsShortENI mirrors host.shortENIID: the FNV-32a hash of the full ENI ID as 8
// hex chars, which the per-tap port names key off (inlined, as this suite inlines
// the other OVS/OVN names). A hash, NOT a truncation — suffix-sharing ENIs differ.
func imdsShortENI(eniID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(eniID))
	return fmt.Sprintf("%08x", h.Sum32())
}

// imdsTapName mirrors vm.TapDeviceName: "tap" + the ENI (sans eni- prefix),
// truncated to the 15-char IFNAMSIZ limit.
func imdsTapName(eniID string) string {
	name := "tap" + strings.TrimPrefix(eniID, "eni-")
	if len(name) > 15 {
		name = name[:15]
	}
	return name
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
	adminPolicyARN := harness.IAMPolicyARN(adminAccount, imdsAdminPolicyName)
	harness.IAMDeleteRoleAndProfilesBestEffort(fix.AWS, imdsRoleName, []string{imdsProfileName}, adminPolicyARN)
	fix.Harness.RegisterCleanup(func() {
		harness.IAMDeleteRoleAndProfilesBestEffort(fix.AWS, imdsRoleName, []string{imdsProfileName}, adminPolicyARN)
	})

	harness.Step(t, "create-role %q (trust=ec2.amazonaws.com) + profile %q", imdsRoleName, imdsProfileName)
	_, err := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(imdsRoleName),
		AssumeRolePolicyDocument: aws.String(imdsTrustPolicyEC2),
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
// out the cold-start window before vpcd's reconcile-from-taps binds the per-tap
// responder. Returns the token. On timeout it dumps the per-tap IMDS datapath
// (br-imds flows/ports + reply routing + listener + conntrack) before failing,
// so a reachability timeout (exit 28) is triaged as request-path vs reply-path
// rather than a bare "condition not met".
func imdsAwaitToken(t *testing.T, fix *Fixture, tgt harness.SSHTarget, subnetID, guestIP string) string {
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
			harness.DumpIMDSDatapathDiagnostics(t, subnetID, guestIP, fix.ArtifactDir(t))
			t.Fatalf("imdsAwaitToken: token never minted within 90s (subnet=%s guest=%s): %v "+
				"(see IMDS datapath diagnostics above)", subnetID, guestIP, lastErr)
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

// imdsExpectedPubKey derives the OpenSSH public key (type + base64) the IMDS
// material path must serve, from the launch key pair's PEM. CreateKeyPair returns
// only the private PEM, so the public key is recomputed from it via ssh.Signer.
// The stored key carries an empty comment (ssh-keygen -C ""), so callers compare
// on the type + base64 fields, not the full line.
func imdsExpectedPubKey(t *testing.T, pemPath string) (keyType, base64Key string) {
	t.Helper()
	pem, err := os.ReadFile(pemPath)
	require.NoErrorf(t, err, "read key PEM %s", pemPath)
	signer, err := ssh.ParsePrivateKey(pem)
	require.NoError(t, err, "parse key PEM")
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	fields := strings.Fields(authorized)
	require.GreaterOrEqualf(t, len(fields), 2, "derived public key malformed: %q", authorized)
	return fields[0], fields[1]
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
