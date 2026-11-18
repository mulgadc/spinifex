//go:build e2e

package single

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// peeringTenant is a throwaway account the test deletes itself, which also
// removes everything created in it.
type peeringTenant struct {
	harness.AccountInfo
	fix     *Fixture
	deleted bool
}

func newPeeringTenant(t *testing.T, fix *Fixture, label string) *peeringTenant {
	t.Helper()
	info := harness.SpxAdminAccountCreate(t, fmt.Sprintf("VPC Peering %s %d", label, time.Now().UnixNano()), "")
	carousel := harness.NewAccountCarousel()
	p := carousel.Add(t, fix.Env, label, info)
	tenantFix := *fix
	tenantFix.AWS = p.Client
	tn := &peeringTenant{AccountInfo: info, fix: &tenantFix}
	t.Cleanup(func() {
		if !tn.deleted {
			out := harness.SpxRunBestEffort(t, "admin", "account", "delete", tn.AccountID, "--yes", "--force", "--name", tn.AccountName)
			t.Logf("cleanup: delete account %s: %s", tn.AccountID, out)
		}
	})
	return tn
}

func (tn *peeringTenant) client() *harness.AWSClient { return tn.fix.AWS }

// remove deletes the account through the account-termination path.
func (tn *peeringTenant) remove(t *testing.T) {
	t.Helper()
	harness.SpxRun(t, false, "admin", "account", "delete", tn.AccountID, "--yes", "--name", tn.AccountName)
	tn.deleted = true
}

// tenantKeyPair creates a key pair in the tenant and writes its private key.
func tenantKeyPair(t *testing.T, c *harness.AWSClient) (name, path string) {
	t.Helper()
	name = fmt.Sprintf("pcx-xacct-%d", time.Now().UnixNano())
	out, err := c.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(name)})
	require.NoError(t, err, "create-key-pair in tenant")
	path = filepath.Join(t.TempDir(), name+".pem")
	require.NoError(t, os.WriteFile(path, []byte(aws.StringValue(out.KeyMaterial)), 0o600))
	return name, path
}

func createCrossAccountPeering(t *testing.T, c *harness.AWSClient, vpc, peerOwner, peer string) (string, *ec2.VpcPeeringConnection) {
	t.Helper()
	out, err := c.EC2.CreateVpcPeeringConnection(&ec2.CreateVpcPeeringConnectionInput{
		VpcId: aws.String(vpc), PeerVpcId: aws.String(peer), PeerOwnerId: aws.String(peerOwner),
	})
	require.NoError(t, err, "create-vpc-peering-connection %s -> %s/%s", vpc, peerOwner, peer)
	id := aws.StringValue(out.VpcPeeringConnection.VpcPeeringConnectionId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteVpcPeeringConnection(&ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)})
	})
	harness.Detail(t, "pcx", id, "requester", vpc, "accepter", peer)
	return id, out.VpcPeeringConnection
}

// describePeering returns the connection as c sees it, or nil.
func describePeering(t *testing.T, c *harness.AWSClient, id string) *ec2.VpcPeeringConnection {
	t.Helper()
	out, err := c.EC2.DescribeVpcPeeringConnections(&ec2.DescribeVpcPeeringConnectionsInput{VpcPeeringConnectionIds: []*string{aws.String(id)}})
	if err != nil {
		requireAWSCode(t, err, "InvalidVpcPeeringConnectionID.NotFound")
		return nil
	}
	require.Len(t, out.VpcPeeringConnections, 1)
	return out.VpcPeeringConnections[0]
}

func peeringTags(conn *ec2.VpcPeeringConnection) map[string]string {
	out := map[string]string{}
	for _, tag := range conn.Tags {
		out[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return out
}

// deniedPrincipal creates an IAM user in root's account allowed every EC2
// action except action on resource, and returns its client once it answers.
func deniedPrincipal(t *testing.T, env *harness.Env, root *harness.AWSClient, action, resource string) *harness.AWSClient {
	t.Helper()
	user := fmt.Sprintf("pcx-deny-%d", time.Now().UnixNano())
	_, err := root.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(user)})
	require.NoError(t, err, "create-user %s", user)
	doc, err := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Action": "ec2:*", "Resource": "*"},
			{"Effect": "Deny", "Action": action, "Resource": resource},
		},
	})
	require.NoError(t, err)
	const policy = "pcx-deny"
	_, err = root.IAM.PutUserPolicy(&iam.PutUserPolicyInput{
		UserName: aws.String(user), PolicyName: aws.String(policy), PolicyDocument: aws.String(string(doc)),
	})
	require.NoError(t, err, "put-user-policy %s", user)
	key, err := root.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(user)})
	require.NoError(t, err, "create-access-key %s", user)
	t.Cleanup(func() {
		_, _ = root.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{UserName: aws.String(user), AccessKeyId: key.AccessKey.AccessKeyId})
		_, _ = root.IAM.DeleteUserPolicy(&iam.DeleteUserPolicyInput{UserName: aws.String(user), PolicyName: aws.String(policy)})
		_, _ = root.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(user)})
	})
	c := harness.NewAWSClientWithCreds(t, env, aws.StringValue(key.AccessKey.AccessKeyId), aws.StringValue(key.AccessKey.SecretAccessKey))
	harness.Eventually(t, func() bool {
		_, err := c.EC2.DescribeVpcPeeringConnections(&ec2.DescribeVpcPeeringConnectionsInput{})
		return err == nil
	}, 60*time.Second, 2*time.Second, "IAM user %s never authenticated", user)
	return c
}

func pcxARN(account, id string) string {
	return fmt.Sprintf("arn:aws:ec2:*:%s:vpc-peering-connection/%s", account, id)
}

// runVPCPeeringCrossAccount proves peering between two accounts: only the
// accepter accepts or rejects, a third account sees nothing, IAM scopes carry
// the caller's account, and DeleteVpc and account termination follow AWS.
func runVPCPeeringCrossAccount(t *testing.T, fix *Fixture) {
	if !fix.PublicPool {
		t.Skip("cross-account peering reaches its guests over public addresses")
	}
	harness.Phase(t, "Single — VPC peering across accounts")
	a := fix.AWS
	aAcct := harness.IAMAccountID(t, a)
	bTen := newPeeringTenant(t, fix, "accepter")
	cTen := newPeeringTenant(t, fix, "unrelated")
	b, c3 := bTen.client(), cTen.client()

	ami := needAMI(t, fix)
	instType, _ := harness.DiscoverNanoInstanceType(t, fix.Harness)
	aKey, aKeyPath := harness.EnsureKeyPair(t, fix.Harness)
	bKey, bKeyPath := tenantKeyPair(t, b)

	ag := newPeeringGuest(t, fix, "10.160.0.0/16", "10.160.1.0/24", ami, instType, aKey, aKeyPath)
	bg := buildPeeringGuest(t, bTen.fix, func(func()) {}, "10.161.0.0/16", "10.161.1.0/24", ami, instType, bKey, bKeyPath)
	requirePingBlocked(t, ag.tgt, bg.privateIP, "before peering")

	pcx, created := createCrossAccountPeering(t, a, ag.vpcID, bTen.AccountID, bg.vpcID)
	require.Equal(t, "initiating-request", aws.StringValue(created.Status.Code))
	require.Equal(t, bTen.AccountID, aws.StringValue(created.AccepterVpcInfo.OwnerId))

	harness.Step(t, "1. only the accepter can accept or reject")
	_, err := a.EC2.AcceptVpcPeeringConnection(&ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	requireAWSCode(t, err, "InvalidVpcPeeringConnectionID.NotFound")
	_, err = a.EC2.RejectVpcPeeringConnection(&ec2.RejectVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	requireAWSCode(t, err, "InvalidVpcPeeringConnectionID.NotFound")
	require.Equal(t, "pending-acceptance", aws.StringValue(describePeering(t, b, pcx).Status.Code))

	harness.Step(t, "3. the unrelated account cannot see, change or route to it")
	require.Nil(t, describePeering(t, c3, pcx), "unrelated account described the connection")
	all, err := c3.EC2.DescribeVpcPeeringConnections(&ec2.DescribeVpcPeeringConnectionsInput{})
	require.NoError(t, err)
	for _, conn := range all.VpcPeeringConnections {
		require.NotEqual(t, pcx, aws.StringValue(conn.VpcPeeringConnectionId), "unrelated account listed the connection")
	}
	_, err = c3.EC2.AcceptVpcPeeringConnection(&ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	requireAWSCode(t, err, "InvalidVpcPeeringConnectionID.NotFound")
	_, err = c3.EC2.RejectVpcPeeringConnection(&ec2.RejectVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	requireAWSCode(t, err, "InvalidVpcPeeringConnectionID.NotFound")
	_, err = c3.EC2.DeleteVpcPeeringConnection(&ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	requireAWSCode(t, err, "InvalidVpcPeeringConnectionID.NotFound")
	_, err = c3.EC2.ModifyVpcPeeringConnectionOptions(&ec2.ModifyVpcPeeringConnectionOptionsInput{
		VpcPeeringConnectionId:            aws.String(pcx),
		RequesterPeeringConnectionOptions: &ec2.PeeringConnectionOptionsRequest{AllowDnsResolutionFromRemoteVpc: aws.Bool(false)},
	})
	requireAWSCode(t, err, "InvalidVpcPeeringConnectionID.NotFound")
	cVpc := harness.CreateVPC(t, c3, "10.162.0.0/16")
	_, err = c3.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId: aws.String(harness.MainRouteTableID(t, c3, cVpc)), DestinationCidrBlock: aws.String(ag.cidr), VpcPeeringConnectionId: aws.String(pcx),
	})
	requireAWSCode(t, err, "InvalidVpcPeeringConnectionID.NotFound")

	harness.Step(t, "4. IAM scopes the connection ARN to the caller's own account")
	bDenied := deniedPrincipal(t, fix.Env, b, "ec2:AcceptVpcPeeringConnection", pcxARN(bTen.AccountID, pcx))
	_, err = bDenied.EC2.AcceptVpcPeeringConnection(&ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	requireAWSCode(t, err, "UnauthorizedOperation")
	bOtherDeny := deniedPrincipal(t, fix.Env, b, "ec2:AcceptVpcPeeringConnection", pcxARN(aAcct, pcx))
	_, err = bOtherDeny.EC2.AcceptVpcPeeringConnection(&ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	require.NoError(t, err, "a deny naming the requester's account must not match the accepter's request")
	harness.Eventually(t, func() bool { return peeringStatus(t, a, pcx) == "active" }, 90*time.Second, 2*time.Second, "%s never became active", pcx)
	aDenied := deniedPrincipal(t, fix.Env, a, "ec2:DeleteVpcPeeringConnection", pcxARN(aAcct, pcx))
	_, err = aDenied.EC2.DeleteVpcPeeringConnection(&ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(pcx)})
	requireAWSCode(t, err, "UnauthorizedOperation")

	harness.Step(t, "2. each owner tags and sees only its own tags")
	for _, side := range []struct {
		c     *harness.AWSClient
		value string
	}{{a, "requester"}, {b, "accepter"}, {c3, "unrelated"}} {
		_, err := side.c.EC2.CreateTags(&ec2.CreateTagsInput{
			Resources: []*string{aws.String(pcx)}, Tags: []*ec2.Tag{{Key: aws.String("side"), Value: aws.String(side.value)}},
		})
		if side.value != "unrelated" {
			require.NoError(t, err, "create-tags as the %s", side.value)
		}
	}
	require.Equal(t, map[string]string{"side": "requester"}, peeringTags(describePeering(t, a, pcx)))
	require.Equal(t, map[string]string{"side": "accepter"}, peeringTags(describePeering(t, b, pcx)))

	harness.Step(t, "6. routes on both sides connect the two accounts' instances")
	peeringRoute(t, a, ag.rtbID, bg.cidr, pcx)
	peeringRoute(t, b, bg.rtbID, ag.cidr, pcx)
	requirePingConverged(t, ag.tgt, bg.privateIP, "requester -> accepter")
	requirePingConverged(t, bg.tgt, ag.privateIP, "accepter -> requester")
	require.Equal(t, "active", routeState(t, b, bg.rtbID, ag.cidr))

	t.Run("FailedPeerVpc", func(t *testing.T) {
		harness.Step(t, "5. a peer VPC the peer account does not own fails like a missing one")
		const missing = "vpc-0123456789abcdef0"
		for _, peer := range []string{missing, cVpc} {
			id, _ := createCrossAccountPeering(t, a, ag.vpcID, bTen.AccountID, peer)
			conn := describePeering(t, a, id)
			require.NotNil(t, conn)
			require.Equal(t, "failed", aws.StringValue(conn.Status.Code), "peer %s", peer)
			require.Equal(t, "Failed due to incorrect VPC-ID, Account ID, overlapping CIDR range, or VPC Encryption Control posture",
				aws.StringValue(conn.Status.Message), "peer %s", peer)
			require.Nil(t, describePeering(t, b, id), "the accepter sees a failed request")
		}
	})

	t.Run("DeleteVpcWithPendingRequest", func(t *testing.T) {
		harness.Step(t, "7. the accepter cannot delete a VPC with a pending request; the requester can")
		aVpc := harness.CreateVPC(t, a, "10.163.0.0/16")
		t.Cleanup(func() { harness.DeleteVPC(t, a, aVpc) })
		bVpc := harness.CreateVPC(t, b, "10.164.0.0/16")
		id, _ := createCrossAccountPeering(t, a, aVpc, bTen.AccountID, bVpc)
		_, err := b.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(bVpc)})
		requireAWSCode(t, err, "DependencyViolation")
		_, err = a.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(aVpc)})
		require.NoError(t, err, "requester DeleteVpc with a pending request")
		for _, owner := range []*harness.AWSClient{a, b} {
			conn := describePeering(t, owner, id)
			require.NotNil(t, conn)
			require.Equal(t, "deleted", aws.StringValue(conn.Status.Code))
		}
		_, err = b.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(bVpc)})
		require.NoError(t, err, "accepter DeleteVpc once the request is deleted")
	})

	harness.Step(t, "8. terminating the accepter account deletes its connections and rejects its pending requests")
	idle := harness.CreateVPC(t, a, "10.165.0.0/16")
	t.Cleanup(func() { harness.DeleteVPC(t, a, idle) })
	bIdle := harness.CreateVPC(t, b, "10.166.0.0/16")
	idlePcx, _ := createCrossAccountPeering(t, a, idle, bTen.AccountID, bIdle)
	_, err = b.EC2.AcceptVpcPeeringConnection(&ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(idlePcx)})
	require.NoError(t, err, "accept the idle connection")
	harness.Eventually(t, func() bool { return peeringStatus(t, a, idlePcx) == "active" }, 90*time.Second, 2*time.Second, "%s never became active", idlePcx)
	waiting := harness.CreateVPC(t, a, "10.167.0.0/16")
	t.Cleanup(func() { harness.DeleteVPC(t, a, waiting) })
	bWaiting := harness.CreateVPC(t, b, "10.168.0.0/16")
	pendingPcx, _ := createCrossAccountPeering(t, a, waiting, bTen.AccountID, bWaiting)
	require.Equal(t, "pending-acceptance", peeringStatus(t, a, pendingPcx))

	bTen.remove(t)
	for _, id := range []string{pcx, idlePcx} {
		require.Equal(t, "deleted", peeringStatus(t, a, id), "%s after the accepter account was deleted", id)
	}
	require.Equal(t, "rejected", peeringStatus(t, a, pendingPcx), "a request pending against the deleted account")
	require.Equal(t, "blackhole", routeState(t, a, ag.rtbID, bg.cidr))
	requirePingBlocked(t, ag.tgt, bg.privateIP, "after the accepter account was deleted")
	_, err = a.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(idle)})
	require.NoError(t, err, "requester VPC deletes once its peer account is gone")
	_, err = a.EC2.DeleteRoute(&ec2.DeleteRouteInput{RouteTableId: aws.String(ag.rtbID), DestinationCidrBlock: aws.String(bg.cidr)})
	require.NoError(t, err, "delete blackhole route")
}
