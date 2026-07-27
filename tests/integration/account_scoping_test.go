//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertAWSErrorCode fails the test unless err is an awserr.Error carrying
// the given code — the same shape tests/e2e/harness.ExpectError checks at the
// live tier.
func assertAWSErrorCode(t *testing.T, err error, code, what string) {
	t.Helper()
	require.Errorf(t, err, "%s: expected an error, got none", what)
	aerr, ok := err.(awserr.Error)
	require.Truef(t, ok, "%s: error is not an awserr.Error: %v", what, err)
	assert.Equalf(t, code, aerr.Code(), "%s: unexpected error code", what)
}

// describeKeyPairID resolves a key-pair name to its KeyPairId, scoped to
// whichever principal ec2Cli is signed for.
func describeKeyPairID(t *testing.T, ec2Cli *ec2.EC2, name string) string {
	t.Helper()
	out, err := ec2Cli.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		KeyNames: []*string{aws.String(name)},
	})
	require.NoError(t, err, "describe-key-pairs %s", name)
	if len(out.KeyPairs) == 0 {
		return ""
	}
	return aws.StringValue(out.KeyPairs[0].KeyPairId)
}

// vpcIDs extracts VpcId from a DescribeVpcs result.
func vpcIDs(vpcs []*ec2.Vpc) []string {
	ids := make([]string, 0, len(vpcs))
	for _, v := range vpcs {
		ids = append(ids, aws.StringValue(v.VpcId))
	}
	return ids
}

// adminAccessPolicyDocument grants unrestricted EC2/IAM/etc. access, mirroring
// cmd/spinifex/cmd/admin.go's runAccountCreate — a freshly minted IAM user has
// no attached policy, so gateway.evaluatePrincipalPolicy denies every call
// (AccessDenied) until one is attached; only the harness's seeded root
// bypasses policy evaluation entirely.
const adminAccessPolicyDocument = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`

// createTenantAccount mints a second real tenant account directly against the
// gateway's IAMService (IAMServiceImpl.CreateAccount/CreateUser/CreateAccessKey/
// CreatePolicy/AttachUserPolicy — the same Go calls
// cmd/spinifex/cmd/admin.go's runAccountCreate makes on top of the spx CLI,
// minus the CLI/stdout round-trip) and returns SDK clients signed with its
// access key. Used to get two independently-owned accounts without the live
// tier's SpxAdminAccountCreate, which shells out to a spx binary this tier
// does not build or run.
func createTenantAccount(t *testing.T, gw *Gateway, name string) *PrincipalClients {
	t.Helper()

	acct, err := gw.Config.IAMService.CreateAccount(name)
	require.NoError(t, err, "create account %s", name)

	_, err = gw.Config.IAMService.CreateUser(acct.AccountID, &iam.CreateUserInput{
		UserName: aws.String("root"),
	})
	require.NoError(t, err, "create user for account %s", name)

	key, err := gw.Config.IAMService.CreateAccessKey(acct.AccountID, &iam.CreateAccessKeyInput{
		UserName: aws.String("root"),
	})
	require.NoError(t, err, "create access key for account %s", name)

	policyOut, err := gw.Config.IAMService.CreatePolicy(acct.AccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("AdministratorAccess"),
		PolicyDocument: aws.String(adminAccessPolicyDocument),
	})
	require.NoError(t, err, "create policy for account %s", name)

	_, err = gw.Config.IAMService.AttachUserPolicy(acct.AccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("root"),
		PolicyArn: policyOut.Policy.Arn,
	})
	require.NoError(t, err, "attach policy for account %s", name)

	return gw.ClientsWithCreds(t, aws.StringValue(key.AccessKey.AccessKeyId), aws.StringValue(key.AccessKey.SecretAccessKey))
}

// TestAccountScoping_KeyPairs, TestAccountScoping_VPCSubnet,
// TestAccountScoping_IGWEIGW and TestAccountScoping_Settings port the
// tests/e2e/single/account_test.go Steps 4/6/7/8 cross-account-isolation
// assertions for resources whose ownership check is a plain account-scoped KV
// lookup (KeyServiceImpl, VPCServiceImpl, IGWServiceImpl,
// EgressOnlyIGWServiceImpl, AccountSettingsServiceImpl — all wired for real
// via DaemonLite, none of them call viperblock.New). Two tenant accounts are
// minted via createTenantAccount so isolation is checked between real,
// independently-owned accounts rather than a single seeded root.
//
// Left live in account_test.go: Step2 (instance scoping) and Step3 (volume
// scoping) — checkInstanceOwnership, the actual cross-account gate for
// stop/start/reboot/terminate/attach-volume/etc., runs daemon-side only after
// a real vm.Manager lookup (daemon/daemon_handlers.go handleEC2Events), so it
// cannot be exercised without a live guest. Step5 (snapshot scoping) needs a
// real backing volume for CreateSnapshot (viperblock, out of DaemonLite's
// scope) — only its cross-account DeleteSnapshot/CreateSnapshot-of-foreign-
// volume checks are KV-only, and porting those alone would still require the
// same real-volume setup, so the whole step stays live. Step9 (global
// resources) and the volume/snapshot-not-found half of Step10 are not ported:
// DescribeRegions/DescribeAvailabilityZones/DescribeInstanceTypes carry no
// account branching at all (same values regardless of caller), so asserting
// their equality across two accounts proves nothing a fake node responder
// wouldn't also trivially satisfy.
func TestAccountScoping_KeyPairs(t *testing.T) {
	gw := StartGateway(t)
	StartDaemonLite(t, gw)

	alpha := createTenantAccount(t, gw, "scoping-alpha")
	beta := createTenantAccount(t, gw, "scoping-beta")

	const alphaKey = "alpha-key"
	const betaKey = "beta-key"
	const sharedKey = "shared-name"

	_, err := alpha.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(alphaKey)})
	require.NoError(t, err, "alpha create-key-pair")
	alphaKeyID := describeKeyPairID(t, alpha.EC2, alphaKey)
	require.NotEmpty(t, alphaKeyID, "alpha key-pair id")

	_, err = beta.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(betaKey)})
	require.NoError(t, err, "beta create-key-pair")

	alphaKeys := describeKeyNames(t, alpha.EC2)
	assert.NotContains(t, alphaKeys, betaKey, "alpha saw beta's key")

	// Same name in both accounts — must resolve to different KeyPairIds.
	_, err = alpha.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(sharedKey)})
	require.NoError(t, err, "alpha create-key-pair %s", sharedKey)
	_, err = beta.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(sharedKey)})
	require.NoError(t, err, "beta create-key-pair %s", sharedKey)
	alphaShared := describeKeyPairID(t, alpha.EC2, sharedKey)
	betaShared := describeKeyPairID(t, beta.EC2, sharedKey)
	require.NotEmpty(t, alphaShared)
	require.NotEmpty(t, betaShared)
	assert.NotEqual(t, alphaShared, betaShared, "same KeyPairId across accounts")

	// Cross-account delete must be a no-op, not an error — AWS's own
	// DeleteKeyPair is idempotent-by-name and never reports whose key it is.
	_, _ = beta.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(alphaKey)})
	assert.Equal(t, alphaKeyID, describeKeyPairID(t, alpha.EC2, alphaKey),
		"beta's delete affected alpha's key")
}

func TestAccountScoping_VPCSubnet(t *testing.T) {
	gw := StartGateway(t)
	StartDaemonLite(t, gw)

	alpha := createTenantAccount(t, gw, "scoping-alpha")
	beta := createTenantAccount(t, gw, "scoping-beta")

	av, err := alpha.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	require.NoError(t, err, "alpha create-vpc")
	alphaVPC := aws.StringValue(av.Vpc.VpcId)
	require.NotEmpty(t, alphaVPC)

	bv, err := beta.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	require.NoError(t, err, "beta create-vpc (same CIDR, no conflict)")
	betaVPC := aws.StringValue(bv.Vpc.VpcId)
	require.NotEmpty(t, betaVPC)

	alphaVPCs, err := alpha.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{})
	require.NoError(t, err, "alpha describe-vpcs")
	assert.NotContains(t, vpcIDs(alphaVPCs.Vpcs), betaVPC, "alpha saw beta's VPC")

	_, err = alpha.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{VpcIds: []*string{aws.String(betaVPC)}})
	assertAWSErrorCode(t, err, "InvalidVpcID.NotFound", "cross-account describe-vpc by id")

	_, err = beta.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(alphaVPC)})
	assertAWSErrorCode(t, err, "InvalidVpcID.NotFound", "cross-account delete-vpc")

	as, err := alpha.EC2.CreateSubnet(&ec2.CreateSubnetInput{VpcId: aws.String(alphaVPC), CidrBlock: aws.String("10.0.1.0/24")})
	require.NoError(t, err, "alpha create-subnet")
	alphaSubnet := aws.StringValue(as.Subnet.SubnetId)
	require.NotEmpty(t, alphaSubnet)

	bs, err := beta.EC2.CreateSubnet(&ec2.CreateSubnetInput{VpcId: aws.String(betaVPC), CidrBlock: aws.String("10.0.1.0/24")})
	require.NoError(t, err, "beta create-subnet")
	betaSubnet := aws.StringValue(bs.Subnet.SubnetId)
	require.NotEmpty(t, betaSubnet)

	alphaSubnets, err := alpha.EC2.DescribeSubnets(&ec2.DescribeSubnetsInput{})
	require.NoError(t, err, "alpha describe-subnets")
	var alphaSubnetIDs []string
	for _, s := range alphaSubnets.Subnets {
		alphaSubnetIDs = append(alphaSubnetIDs, aws.StringValue(s.SubnetId))
	}
	assert.NotContains(t, alphaSubnetIDs, betaSubnet, "alpha saw beta's subnet")

	_, err = beta.EC2.CreateSubnet(&ec2.CreateSubnetInput{VpcId: aws.String(alphaVPC), CidrBlock: aws.String("10.0.2.0/24")})
	assertAWSErrorCode(t, err, "InvalidVpcID.NotFound", "cross-account create-subnet in other VPC")

	_, err = beta.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(alphaSubnet)})
	assertAWSErrorCode(t, err, "InvalidSubnetID.NotFound", "cross-account delete-subnet")
}

func TestAccountScoping_IGWEIGW(t *testing.T) {
	gw := StartGateway(t)
	StartDaemonLite(t, gw)

	alpha := createTenantAccount(t, gw, "scoping-alpha")
	beta := createTenantAccount(t, gw, "scoping-beta")

	av, err := alpha.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	require.NoError(t, err, "alpha create-vpc")
	alphaVPC := aws.StringValue(av.Vpc.VpcId)

	bv, err := beta.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	require.NoError(t, err, "beta create-vpc")
	betaVPC := aws.StringValue(bv.Vpc.VpcId)

	ai, err := alpha.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "alpha create-internet-gateway")
	alphaIGW := aws.StringValue(ai.InternetGateway.InternetGatewayId)
	require.NotEmpty(t, alphaIGW)

	bi, err := beta.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "beta create-internet-gateway")
	betaIGW := aws.StringValue(bi.InternetGateway.InternetGatewayId)
	require.NotEmpty(t, betaIGW)

	alphaIGWs, err := alpha.EC2.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{})
	require.NoError(t, err, "alpha describe-internet-gateways")
	var alphaIGWIDs []string
	for _, ig := range alphaIGWs.InternetGateways {
		alphaIGWIDs = append(alphaIGWIDs, aws.StringValue(ig.InternetGatewayId))
	}
	assert.NotContains(t, alphaIGWIDs, betaIGW, "alpha saw beta's IGW")

	_, err = alpha.EC2.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{InternetGatewayIds: []*string{aws.String(betaIGW)}})
	assertAWSErrorCode(t, err, "InvalidInternetGatewayID.NotFound", "cross-account describe IGW by id")

	_, err = beta.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String(alphaIGW)})
	assertAWSErrorCode(t, err, "InvalidInternetGatewayID.NotFound", "cross-account delete IGW")

	_, err = alpha.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{InternetGatewayId: aws.String(betaIGW), VpcId: aws.String(alphaVPC)})
	assertAWSErrorCode(t, err, "InvalidInternetGatewayID.NotFound", "cross-account attach IGW (alpha attaches beta's IGW)")

	_, err = alpha.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{InternetGatewayId: aws.String(alphaIGW), VpcId: aws.String(alphaVPC)})
	require.NoError(t, err, "alpha attach own IGW to own VPC")

	_, err = beta.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{InternetGatewayId: aws.String(alphaIGW), VpcId: aws.String(alphaVPC)})
	assertAWSErrorCode(t, err, "InvalidInternetGatewayID.NotFound", "cross-account detach IGW")

	ae, err := alpha.EC2.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{VpcId: aws.String(alphaVPC)})
	require.NoError(t, err, "alpha create-eigw")
	alphaEIGW := aws.StringValue(ae.EgressOnlyInternetGateway.EgressOnlyInternetGatewayId)
	require.NotEmpty(t, alphaEIGW)

	be, err := beta.EC2.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{VpcId: aws.String(betaVPC)})
	require.NoError(t, err, "beta create-eigw")
	betaEIGW := aws.StringValue(be.EgressOnlyInternetGateway.EgressOnlyInternetGatewayId)
	require.NotEmpty(t, betaEIGW)

	alphaEIGWs, err := alpha.EC2.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{})
	require.NoError(t, err, "alpha describe-eigws")
	var alphaEIGWIDs []string
	for _, ig := range alphaEIGWs.EgressOnlyInternetGateways {
		alphaEIGWIDs = append(alphaEIGWIDs, aws.StringValue(ig.EgressOnlyInternetGatewayId))
	}
	assert.NotContains(t, alphaEIGWIDs, betaEIGW, "alpha saw beta's EIGW")

	// Cross-account delete must not remove alpha's EIGW.
	_, _ = beta.EC2.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{EgressOnlyInternetGatewayId: aws.String(alphaEIGW)})
	check, err := alpha.EC2.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{})
	require.NoError(t, err, "alpha describe-eigws (post cross-account delete attempt)")
	var stillThere bool
	for _, ig := range check.EgressOnlyInternetGateways {
		if aws.StringValue(ig.EgressOnlyInternetGatewayId) == alphaEIGW {
			stillThere = true
			break
		}
	}
	assert.True(t, stillThere, "alpha's EIGW was deleted by beta")
}

func TestAccountScoping_Settings(t *testing.T) {
	gw := StartGateway(t)
	StartDaemonLite(t, gw)

	alpha := createTenantAccount(t, gw, "scoping-alpha")
	beta := createTenantAccount(t, gw, "scoping-beta")

	_, err := alpha.EC2.EnableEbsEncryptionByDefault(&ec2.EnableEbsEncryptionByDefaultInput{})
	require.NoError(t, err, "alpha enable-ebs-encryption")

	betaEnc, err := beta.EC2.GetEbsEncryptionByDefault(&ec2.GetEbsEncryptionByDefaultInput{})
	require.NoError(t, err, "beta get-ebs-encryption")
	assert.False(t, aws.BoolValue(betaEnc.EbsEncryptionByDefault), "alpha's encryption setting leaked to beta")

	// Independent toggle: enable beta, disable alpha.
	_, err = beta.EC2.EnableEbsEncryptionByDefault(&ec2.EnableEbsEncryptionByDefaultInput{})
	require.NoError(t, err, "beta enable-ebs-encryption")
	_, err = alpha.EC2.DisableEbsEncryptionByDefault(&ec2.DisableEbsEncryptionByDefaultInput{})
	require.NoError(t, err, "alpha disable-ebs-encryption")

	alphaEnc, err := alpha.EC2.GetEbsEncryptionByDefault(&ec2.GetEbsEncryptionByDefaultInput{})
	require.NoError(t, err, "alpha get-ebs-encryption")
	betaEnc, err = beta.EC2.GetEbsEncryptionByDefault(&ec2.GetEbsEncryptionByDefaultInput{})
	require.NoError(t, err, "beta get-ebs-encryption")
	assert.False(t, aws.BoolValue(alphaEnc.EbsEncryptionByDefault), "alpha encryption should be off")
	assert.True(t, aws.BoolValue(betaEnc.EbsEncryptionByDefault), "beta encryption should be on")
}
