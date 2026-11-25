//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/require"
)

// S3EndpointActivationBudget bounds the wait for the formation to offer the S3
// gateway endpoint service, which needs every node's capture probe to pass.
var S3EndpointActivationBudget = durationEnv("SPINIFEX_VPCE_ACTIVATION_TIMEOUT", 2*time.Minute)

// S3GatePort is the port the S3 prefix-list entries serve on. The API never
// renders it, so the harness takes it from SPINIFEX_S3_GATE_PORT.
func S3GatePort() int { return atoiOr("SPINIFEX_S3_GATE_PORT", predastoreHealthPort) }

// WaitForS3EndpointService polls DescribeVpcEndpointServices until the S3
// service is listed and returns its name, failing with the reason if it never is.
func WaitForS3EndpointService(t *testing.T, c *AWSClient) string {
	t.Helper()
	deadline := time.Now().Add(S3EndpointActivationBudget)
	var last string
	for {
		out, err := c.EC2.DescribeVpcEndpointServices(&ec2.DescribeVpcEndpointServicesInput{})
		if err != nil {
			last = fmt.Sprintf("describe-vpc-endpoint-services: %v", err)
		} else {
			for _, n := range out.ServiceNames {
				name := aws.StringValue(n)
				if strings.HasPrefix(name, "com.amazonaws.") && strings.HasSuffix(name, ".s3") {
					return name
				}
			}
			last = fmt.Sprintf("services listed: %v", aws.StringValueSlice(out.ServiceNames))
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the S3 gateway endpoint service was not offered within %dms (%s): the cluster "+
				"feature level never reached the endpoint level, so some node's vpcd capture probe "+
				"failed or its Predastore does not serve the endpoint listener",
				S3EndpointActivationBudget.Milliseconds(), last)
		}
		time.Sleep(5 * time.Second)
	}
}

// S3EndpointRegion is the region segment of an S3 endpoint service name.
func S3EndpointRegion(serviceName string) string {
	return strings.TrimSuffix(strings.TrimPrefix(serviceName, "com.amazonaws."), ".s3")
}

// S3PrefixList returns the ID of the managed prefix list named serviceName and
// its entries' addresses, requiring every entry to be a /32.
func S3PrefixList(t *testing.T, c *AWSClient, serviceName string) (string, []string) {
	t.Helper()
	pls, err := c.EC2.DescribeManagedPrefixLists(&ec2.DescribeManagedPrefixListsInput{
		Filters: []*ec2.Filter{{Name: aws.String("prefix-list-name"), Values: []*string{aws.String(serviceName)}}},
	})
	require.NoError(t, err, "describe-managed-prefix-lists %s", serviceName)
	require.Len(t, pls.PrefixLists, 1, "want exactly one managed prefix list named %s", serviceName)
	plID := aws.StringValue(pls.PrefixLists[0].PrefixListId)
	require.True(t, strings.HasPrefix(plID, "pl-"), "prefix list ID %q", plID)

	ents, err := c.EC2.GetManagedPrefixListEntries(&ec2.GetManagedPrefixListEntriesInput{PrefixListId: aws.String(plID)})
	require.NoError(t, err, "get-managed-prefix-list-entries %s", plID)
	var addrs []string
	for _, e := range ents.Entries {
		p, perr := netip.ParsePrefix(aws.StringValue(e.Cidr))
		require.NoError(t, perr, "prefix list entry %q", aws.StringValue(e.Cidr))
		require.Equal(t, 32, p.Bits(), "prefix list entry %s is not a /32", p)
		addrs = append(addrs, p.Addr().String())
	}
	require.NotEmpty(t, addrs, "prefix list %s has no entries", plID)
	return plID, addrs
}

// CreateS3GatewayEndpoint creates a Gateway endpoint for serviceName on
// rtbIDs and registers its deletion, which waits for the record to go.
func CreateS3GatewayEndpoint(t *testing.T, c *AWSClient, vpcID, serviceName string, rtbIDs ...string) *ec2.VpcEndpoint {
	t.Helper()
	out, err := c.EC2.CreateVpcEndpoint(&ec2.CreateVpcEndpointInput{
		VpcId:           aws.String(vpcID),
		ServiceName:     aws.String(serviceName),
		VpcEndpointType: aws.String(ec2.VpcEndpointTypeGateway),
		RouteTableIds:   aws.StringSlice(rtbIDs),
	})
	require.NoError(t, err, "create-vpc-endpoint %s in %s", serviceName, vpcID)
	require.NotNil(t, out.VpcEndpoint, "create-vpc-endpoint returned no VpcEndpoint")
	id := aws.StringValue(out.VpcEndpoint.VpcEndpointId)
	require.True(t, strings.HasPrefix(id, "vpce-"), "VpcEndpointId %q", id)
	t.Cleanup(func() {
		if err := DeleteVpcEndpointWait(c, id, 2*time.Minute); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	return out.VpcEndpoint
}

// DeleteVpcEndpointWait deletes id and polls until it is no longer described.
// Cleanup-safe: it never fails the test, and an endpoint already gone is success.
func DeleteVpcEndpointWait(c *AWSClient, id string, timeout time.Duration) error {
	out, err := c.EC2.DeleteVpcEndpoints(&ec2.DeleteVpcEndpointsInput{VpcEndpointIds: []*string{aws.String(id)}})
	if err != nil && !ErrorCodeIs(err, "InvalidVpcEndpointId.NotFound") {
		return fmt.Errorf("delete-vpc-endpoints %s: %w", id, err)
	}
	if err == nil {
		for _, u := range out.Unsuccessful {
			if u.Error != nil && !strings.HasSuffix(aws.StringValue(u.Error.Code), ".NotFound") {
				return fmt.Errorf("delete-vpc-endpoints %s unsuccessful: %s", id, aws.StringValue(u.Error.Message))
			}
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		if _, live, derr := DescribeVpcEndpoint(c, id); derr == nil && !live {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("vpc endpoint %s still described %dms after delete", id, timeout.Milliseconds())
		}
		time.Sleep(2 * time.Second)
	}
}

// DescribeVpcEndpoint returns id's record and whether it is still live; a
// NotFound answer or a deleted state reads as not live.
func DescribeVpcEndpoint(c *AWSClient, id string) (*ec2.VpcEndpoint, bool, error) {
	out, err := c.EC2.DescribeVpcEndpoints(&ec2.DescribeVpcEndpointsInput{VpcEndpointIds: []*string{aws.String(id)}})
	if ErrorCodeIs(err, "InvalidVpcEndpointId.NotFound") {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("describe-vpc-endpoints %s: %w", id, err)
	}
	for _, e := range out.VpcEndpoints {
		if aws.StringValue(e.VpcEndpointId) != id {
			continue
		}
		state := strings.ToLower(aws.StringValue(e.State))
		return e, state != "deleted" && state != "deleting", nil
	}
	return nil, false, nil
}

// WaitVpcEndpointRouteTables polls until id lists exactly want, in any order.
func WaitVpcEndpointRouteTables(t *testing.T, c *AWSClient, id string, want ...string) {
	t.Helper()
	want = slices.Clone(want)
	slices.Sort(want)
	EventuallyErr(t, func() error {
		e, live, err := DescribeVpcEndpoint(c, id)
		if err != nil {
			return err
		}
		if !live {
			return fmt.Errorf("vpc endpoint %s is not described", id)
		}
		got := aws.StringValueSlice(e.RouteTableIds)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			return fmt.Errorf("vpc endpoint %s route tables %v, want %v", id, got, want)
		}
		return nil
	}, 30*time.Second, 2*time.Second)
}

// EndpointRoute returns the route rtbID shows for plID, or nil if it shows none.
func EndpointRoute(t *testing.T, c *AWSClient, rtbID, plID string) *ec2.Route {
	t.Helper()
	out, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{RouteTableIds: []*string{aws.String(rtbID)}})
	require.NoError(t, err, "describe-route-tables %s", rtbID)
	require.Len(t, out.RouteTables, 1, "describe-route-tables %s", rtbID)
	for _, r := range out.RouteTables[0].Routes {
		if aws.StringValue(r.DestinationPrefixListId) == plID {
			return r
		}
	}
	return nil
}

// DeleteVPCWhenFree retries DeleteVpc while it answers DependencyViolation,
// for teardown behind resources whose deletion converges. Never fails the test.
func DeleteVPCWhenFree(t *testing.T, c *AWSClient, vpcID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
		if err == nil || ErrorCodeIs(err, "InvalidVpcID.NotFound") {
			return
		}
		if !ErrorCodeIs(err, "DependencyViolation") || !time.Now().Before(deadline) {
			t.Logf("cleanup: delete-vpc %s: %v", vpcID, err)
			return
		}
		time.Sleep(3 * time.Second)
	}
}

// S3Principal is an IAM user and access key for signing S3 requests. Its
// key material must never be logged.
type S3Principal struct {
	UserName        string
	AccessKeyID     string
	SecretAccessKey string
}

// CreateS3Principal creates an IAM user holding an inline policy of
// statements, with one access key, and registers their deletion.
func CreateS3Principal(t *testing.T, c *AWSClient, userName string, statements []map[string]any) S3Principal {
	t.Helper()
	_, err := c.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-user %s", userName)
	t.Cleanup(func() {
		if _, err := c.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(userName)}); err != nil {
			t.Logf("cleanup: delete-user %s: %v", userName, err)
		}
	})
	doc, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": statements})
	require.NoError(t, err, "marshal policy for %s", userName)
	const policyName = "vpce-e2e"
	_, err = c.IAM.PutUserPolicy(&iam.PutUserPolicyInput{
		UserName: aws.String(userName), PolicyName: aws.String(policyName), PolicyDocument: aws.String(string(doc)),
	})
	require.NoError(t, err, "put-user-policy %s", userName)
	t.Cleanup(func() {
		if _, err := c.IAM.DeleteUserPolicy(&iam.DeleteUserPolicyInput{
			UserName: aws.String(userName), PolicyName: aws.String(policyName),
		}); err != nil {
			t.Logf("cleanup: delete-user-policy %s: %v", userName, err)
		}
	})
	key, err := c.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-access-key %s", userName)
	t.Cleanup(func() {
		if _, err := c.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{
			UserName: aws.String(userName), AccessKeyId: key.AccessKey.AccessKeyId,
		}); err != nil {
			t.Logf("cleanup: delete-access-key for %s: %v", userName, err)
		}
	})
	return S3Principal{
		UserName:        userName,
		AccessKeyID:     aws.StringValue(key.AccessKey.AccessKeyId),
		SecretAccessKey: aws.StringValue(key.AccessKey.SecretAccessKey),
	}
}

// S3BucketStatements grants every S3 action on bucket and its objects.
func S3BucketStatements(bucket string) []map[string]any {
	return []map[string]any{{
		"Effect":   "Allow",
		"Action":   "s3:*",
		"Resource": []string{"arn:aws:s3:::" + bucket, "arn:aws:s3:::" + bucket + "/*"},
	}}
}

// CreateS3BucketWithObject creates bucket holding key as p, against the gate
// at host, once p's key authenticates, and registers the bucket's deletion.
func CreateS3BucketWithObject(t *testing.T, host string, p S3Principal, bucket, key string) {
	t.Helper()
	cli, err := PredastoreS3WithCreds(host, p.AccessKeyID, p.SecretAccessKey)
	require.NoError(t, err, "s3 client for %s", p.UserName)
	EventuallyErr(t, func() error {
		_, cerr := cli.CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(bucket)})
		if cerr == nil || ErrorCodeIs(cerr, "BucketAlreadyOwnedByYou") {
			return nil
		}
		return fmt.Errorf("create-bucket %s as %s: %w", bucket, p.UserName, cerr)
	}, 60*time.Second, 2*time.Second)
	t.Cleanup(func() {
		_, _ = cli.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if _, err := cli.DeleteBucket(&s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Logf("cleanup: delete-bucket %s: %v", bucket, err)
		}
	})
	_, err = cli.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("vpc endpoint e2e\n"),
	})
	require.NoError(t, err, "put-object %s/%s", bucket, key)
}

// WaitS3PrincipalLive waits until p's key authenticates against bucket at
// host, so a later denial is the policy and not a key still propagating.
func WaitS3PrincipalLive(t *testing.T, host string, p S3Principal, bucket string) error {
	t.Helper()
	cli, err := PredastoreS3WithCreds(host, p.AccessKeyID, p.SecretAccessKey)
	require.NoError(t, err, "s3 client for %s", p.UserName)
	var lastErr error
	EventuallyErr(t, func() error {
		_, lastErr = cli.ListObjectsV2(&s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if lastErr == nil || ErrorCodeIs(lastErr, "AccessDenied") {
			return nil
		}
		return fmt.Errorf("key for %s does not authenticate yet: %w", p.UserName, lastErr)
	}, 60*time.Second, 2*time.Second)
	return lastErr
}

// GuestS3CurlConfigCmd writes p's key to a curl config at path in the guest,
// readable only by its owner. Run it with a helper that does not log commands.
func GuestS3CurlConfigCmd(path string, p S3Principal) string {
	line := fmt.Sprintf(`user = "%s:%s"`, p.AccessKeyID, p.SecretAccessKey)
	return fmt.Sprintf("umask 077 && printf '%%s\\n' %s > %s", ShellQuote(line), ShellQuote(path))
}

// GuestCurl is one curl probe's outcome inside a guest: the HTTP status ("000"
// when none arrived), curl's exit code, and the start of the response body.
type GuestCurl struct {
	Code string
	Exit int
	Body string
}

// Reached reports whether the gate answered with any HTTP status.
func (g GuestCurl) Reached() bool { return len(g.Code) == 3 && g.Code != "000" }

// TimedOut reports a connection that never completed: the SYN went nowhere.
func (g GuestCurl) TimedOut() bool { return !g.Reached() && g.Exit == 28 }

// Reset reports a connection refused or reset before any HTTP status.
func (g GuestCurl) Reset() bool { return !g.Reached() && slices.Contains([]int{7, 35, 56}, g.Exit) }

func (g GuestCurl) String() string {
	return fmt.Sprintf("http_code=%s curl_exit=%d", g.Code, g.Exit)
}

// GuestS3ListCmd is a guest command that lists bucket at addr:port signed with
// the curl config at cfgPath. The command always exits 0; parse it with ParseGuestCurl.
func GuestS3ListCmd(cfgPath, region, addr string, port int, bucket string) string {
	return guestCurlCmd(cfgPath, region, "", "", s3URL(addr, port, bucket+"?list-type=2"))
}

func s3URL(addr string, port int, path string) string {
	return "https://" + net.JoinHostPort(addr, strconv.Itoa(port)) + "/" + path
}

// guestCurlCmd runs setup, then one signed curl to url with extra arguments,
// and prints the outcome ParseGuestCurl reads. It always exits 0.
func guestCurlCmd(cfgPath, region, setup, extra, url string) string {
	return fmt.Sprintf(
		"rm -f /tmp/vpce-body; %scode=$(curl -sk -K %s --aws-sigv4 %s --connect-timeout 5 --max-time 10 %s"+
			"-o /tmp/vpce-body -w '%%{http_code}' %s); rc=$?; printf 'vpce-probe %%s %%s\\n' \"$code\" \"$rc\"; "+
			"head -c 4096 /tmp/vpce-body 2>/dev/null; true",
		setup, ShellQuote(cfgPath), ShellQuote("aws:amz:"+region+":s3"), extra, ShellQuote(url))
}

// ParseGuestCurl reads the outcome GuestS3ListCmd printed.
func ParseGuestCurl(out string) (GuestCurl, error) {
	idx := strings.Index(out, "vpce-probe ")
	if idx < 0 {
		return GuestCurl{}, fmt.Errorf("no probe result in guest output %q", out)
	}
	line, body, _ := strings.Cut(out[idx:], "\n")
	f := strings.Fields(line)
	if len(f) != 3 {
		return GuestCurl{}, fmt.Errorf("malformed probe result %q", line)
	}
	rc, err := strconv.Atoi(f[2])
	if err != nil {
		return GuestCurl{}, fmt.Errorf("probe exit code %q: %w", f[2], err)
	}
	return GuestCurl{Code: f[1], Exit: rc, Body: body}, nil
}

// BastionHopArgs are the ssh(1) arguments that run cmd on privIP by hopping
// through bastion, which must hold the guest key at /tmp/key.pem.
func BastionHopArgs(bastion SSHTarget, privIP, cmd string) []string {
	hop := "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR " +
		"-o ConnectTimeout=10 -o BatchMode=yes -o ServerAliveInterval=5 -i /tmp/key.pem ubuntu@" +
		privIP + " " + ShellQuote(cmd)
	return guestSSHArgs(bastion, 10, hop)
}

// GuestExecViaBastion runs cmd on privIP through bastion within timeout and
// returns combined output. It never logs cmd, so cmd may carry a secret.
func GuestExecViaBastion(bastion SSHTarget, privIP, cmd string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "ssh", BastionHopArgs(bastion, privIP, cmd)...)
	c.WaitDelay = 5 * time.Second
	out, err := c.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("guest %s via %s exceeded %dms: %w", privIP, bastion.Host, timeout.Milliseconds(), ctx.Err())
	}
	if err != nil {
		return string(out), fmt.Errorf("guest %s via %s: %w", privIP, bastion.Host, err)
	}
	return string(out), nil
}

// ProbeGuestS3 runs one guest S3 command, such as GuestS3ListCmd, on privIP
// through bastion.
func ProbeGuestS3(bastion SSHTarget, privIP, cmd string) (GuestCurl, error) {
	out, err := GuestExecViaBastion(bastion, privIP, cmd, 45*time.Second)
	if err != nil {
		return GuestCurl{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return ParseGuestCurl(out)
}

// WaitGuestS3 polls ProbeGuestS3 until ok accepts the outcome within budget,
// returning the last outcome and how long it took in milliseconds.
func WaitGuestS3(t *testing.T, budget time.Duration, probe func() (GuestCurl, error), ok func(GuestCurl) bool, what string) (GuestCurl, int64) {
	t.Helper()
	start := time.Now()
	var last GuestCurl
	EventuallyErr(t, func() error {
		g, err := probe()
		if err != nil {
			return err
		}
		last = g
		if ok(g) {
			return nil
		}
		return fmt.Errorf("guest probe %s, want %s", g, what)
	}, budget, 2*time.Second)
	return last, time.Since(start).Milliseconds()
}

// GuestS3DeleteObjectsCmd is a guest command that deletes keys from bucket in
// one DeleteObjects request at addr:port signed with the curl config at
// cfgPath. The command always exits 0; parse it with ParseGuestCurl.
func GuestS3DeleteObjectsCmd(cfgPath, region, addr string, port int, bucket string, keys []string) string {
	var body strings.Builder
	body.WriteString("<Delete>")
	for _, k := range keys {
		body.WriteString("<Object><Key>" + k + "</Key></Object>")
	}
	body.WriteString("</Delete>")
	setup := "printf '%s' " + ShellQuote(body.String()) + " > /tmp/vpce-delete.xml; " +
		"sha=$(sha256sum /tmp/vpce-delete.xml | cut -d' ' -f1); md5=$(openssl dgst -md5 -binary /tmp/vpce-delete.xml | base64); "
	extra := "-X POST -H \"x-amz-content-sha256: $sha\" -H \"Content-MD5: $md5\" -H 'Content-Type: application/xml' " +
		"--data-binary @/tmp/vpce-delete.xml "
	return guestCurlCmd(cfgPath, region, setup, extra, s3URL(addr, port, bucket+"?delete="))
}
