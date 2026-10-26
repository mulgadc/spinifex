//go:build e2e

package route53

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	r53 "github.com/aws/aws-sdk-go/service/route53"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const managedPolicyARN = "arn:aws:iam::aws:policy/"

// grant is what one test principal holds: an AWS managed policy, or an inline
// document.
type grant struct {
	managed string
	inline  map[string]any
}

// IAM users in a tenant account get exactly the Route 53 access their policy
// grants, as in AWS: AmazonRoute53FullAccess manages zones and records and
// nothing else, AmazonRoute53ReadOnlyAccess reads but cannot change or verify,
// and a user granted only S3 is refused every Route 53 call.
func TestRoute53ScopedIAMUsers(t *testing.T) {
	env := harness.LoadEnv(t)
	tenant := newTenant(t, env, "r53-iam")
	name := fmt.Sprintf("z%d.iam.r53e2e.test", time.Now().UnixNano()%1_000_000_000)

	full := scopedUser(t, env, tenant.Client.IAM, "r53-full", grant{managed: "AmazonRoute53FullAccess"})
	reader := scopedUser(t, env, tenant.Client.IAM, "r53-read", grant{managed: "AmazonRoute53ReadOnlyAccess"})
	s3Only := scopedUser(t, env, tenant.Client.IAM, "s3-only", grant{inline: map[string]any{
		"Version":   "2012-10-17",
		"Statement": []map[string]any{{"Effect": "Allow", "Action": "s3:*", "Resource": "*"}},
	}})

	var zoneID string
	// The zone outlives this subtest, so its cleanup belongs to the parent.
	parent := t
	t.Run("FullAccessManagesZonesAndRecords", func(t *testing.T) {
		c := full.Route53
		created, err := c.CreateHostedZone(&r53.CreateHostedZoneInput{Name: aws.String(name), CallerReference: aws.String(name)})
		require.NoError(t, err)
		zoneID = aws.StringValue(created.HostedZone.Id)
		parent.Cleanup(func() { deleteZone(parent, tenant.Client.Route53, zoneID) })
		waitInSync(t, c, created.ChangeInfo.Id)

		change, err := c.ChangeResourceRecordSets(&r53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(zoneID),
			ChangeBatch: &r53.ChangeBatch{Changes: []*r53.Change{
				rrset("CREATE", "www."+name, "A", wwwAddr),
				rrset("CREATE", name, "TXT", `"`+txtMark+`"`),
				rrset("CREATE", name, "MX", fmt.Sprintf("%d mail.%s.", mxPref, name)),
			}},
		})
		require.NoError(t, err)
		waitInSync(t, c, change.ChangeInfo.Id)

		sets, err := c.ListResourceRecordSets(&r53.ListResourceRecordSetsInput{HostedZoneId: aws.String(zoneID)})
		require.NoError(t, err)
		var types []string
		for _, s := range sets.ResourceRecordSets {
			types = append(types, aws.StringValue(s.Type))
		}
		assert.Subset(t, types, []string{"A", "TXT", "MX", "NS", "SOA"})

		_, err = callActivation(c, zoneID, false)
		require.NoError(t, err)
		_, err = callActivation(c, zoneID, true)
		require.NoError(t, err, "AmazonRoute53FullAccess must allow a delegation check")

		_, err = full.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
		harness.AssertAWSError(t, err, "UnauthorizedOperation")
		_, err = full.IAM.ListUsers(&iam.ListUsersInput{})
		harness.AssertAWSError(t, err, "AccessDenied")
	})
	require.NotEmpty(t, zoneID, "the full-access user must have created the zone")
	id := aws.String(zoneID)

	t.Run("ReadOnlyAccessReadsButCannotWrite", func(t *testing.T) {
		c := reader.Route53
		listed, err := c.ListHostedZones(&r53.ListHostedZonesInput{})
		require.NoError(t, err)
		assert.True(t, hasZone(listed.HostedZones, zoneID), "a reader must list the account's zones")
		_, err = c.GetHostedZone(&r53.GetHostedZoneInput{Id: id})
		require.NoError(t, err)
		_, err = c.ListResourceRecordSets(&r53.ListResourceRecordSetsInput{HostedZoneId: id})
		require.NoError(t, err)
		_, err = callActivation(c, zoneID, false)
		require.NoError(t, err, "route53:Get* covers the activation state")

		_, err = c.ChangeResourceRecordSets(&r53.ChangeResourceRecordSetsInput{
			HostedZoneId: id,
			ChangeBatch:  &r53.ChangeBatch{Changes: []*r53.Change{rrset("CREATE", "api."+name, "A", apiAddr)}},
		})
		harness.AssertAWSError(t, err, "AccessDenied")
		_, err = callActivation(c, zoneID, true)
		harness.AssertAWSError(t, err, "AccessDenied")
		_, err = c.CreateHostedZone(&r53.CreateHostedZoneInput{Name: aws.String("ro-" + name), CallerReference: aws.String("ro-" + name)})
		harness.AssertAWSError(t, err, "AccessDenied")
		_, err = c.DeleteHostedZone(&r53.DeleteHostedZoneInput{Id: id})
		harness.AssertAWSError(t, err, "AccessDenied")
	})

	t.Run("S3OnlyUserIsRefusedRoute53", func(t *testing.T) {
		c := s3Only.Route53
		_, err := c.ListHostedZones(&r53.ListHostedZonesInput{})
		harness.AssertAWSError(t, err, "AccessDenied")
		_, err = c.GetHostedZone(&r53.GetHostedZoneInput{Id: id})
		harness.AssertAWSError(t, err, "AccessDenied")
		_, err = c.CreateHostedZone(&r53.CreateHostedZoneInput{Name: aws.String("s3-" + name), CallerReference: aws.String("s3-" + name)})
		harness.AssertAWSError(t, err, "AccessDenied")
		_, err = c.ChangeResourceRecordSets(&r53.ChangeResourceRecordSetsInput{
			HostedZoneId: id,
			ChangeBatch:  &r53.ChangeBatch{Changes: []*r53.Change{rrset("CREATE", "s3."+name, "A", apiAddr)}},
		})
		harness.AssertAWSError(t, err, "AccessDenied")
		_, err = callActivation(c, zoneID, false)
		harness.AssertAWSError(t, err, "AccessDenied")

		// The denials above are the grant, not a dead key: the same user's S3
		// grant works.
		cli, err := harness.PredastoreS3WithCreds(harness.DNSTargets(env)[0].Addr,
			s3Only.key, s3Only.secret)
		require.NoError(t, err)
		_, err = cli.ListBuckets(&s3.ListBucketsInput{})
		require.NoError(t, err, "s3:* must allow ListBuckets")
	})

	// AWS lets ChangeResourceRecordSets be scoped by record name and type. We do
	// not evaluate those keys yet, so a policy naming one is refused when it is
	// written rather than accepted and silently ignored.
	t.Run("UnsupportedRoute53ConditionKeysAreRefused", func(t *testing.T) {
		doc, err := json.Marshal(map[string]any{
			"Version": "2012-10-17",
			"Statement": []map[string]any{{
				"Effect": "Allow", "Action": "route53:ChangeResourceRecordSets", "Resource": "*",
				"Condition": map[string]any{"ForAllValues:StringEquals": map[string]any{
					"route53:ChangeResourceRecordSetsRecordTypes": []string{"TXT"},
				}},
			}},
		})
		require.NoError(t, err)
		_, err = tenant.Client.IAM.PutUserPolicy(&iam.PutUserPolicyInput{
			UserName: aws.String(full.userName), PolicyName: aws.String("r53-txt-only"), PolicyDocument: aws.String(string(doc)),
		})
		harness.AssertAWSError(t, err, "MalformedPolicyDocument")
	})
}

func hasZone(zones []*r53.HostedZone, id string) bool {
	for _, z := range zones {
		if aws.StringValue(z.Id) == id {
			return true
		}
	}
	return false
}

type principal struct {
	*harness.AWSClient
	userName, key, secret string
}

// scopedUser creates an IAM user in the account c administers, holding only g,
// with an access key, and waits until that key authenticates.
func scopedUser(t *testing.T, env *harness.Env, c *iam.IAM, name string, g grant) principal {
	t.Helper()
	user := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	_, err := c.CreateUser(&iam.CreateUserInput{UserName: aws.String(user)})
	require.NoError(t, err, "create-user %s", user)
	t.Cleanup(func() {
		if _, err := c.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(user)}); err != nil {
			t.Logf("cleanup: delete-user %s: %v", user, err)
		}
	})

	if g.managed != "" {
		arn := aws.String(managedPolicyARN + g.managed)
		_, err = c.AttachUserPolicy(&iam.AttachUserPolicyInput{UserName: aws.String(user), PolicyArn: arn})
		require.NoError(t, err, "attach %s to %s", g.managed, user)
		t.Cleanup(func() {
			if _, err := c.DetachUserPolicy(&iam.DetachUserPolicyInput{UserName: aws.String(user), PolicyArn: arn}); err != nil {
				t.Logf("cleanup: detach %s: %v", g.managed, err)
			}
		})
	}
	if g.inline != nil {
		doc, err := json.Marshal(g.inline)
		require.NoError(t, err)
		_, err = c.PutUserPolicy(&iam.PutUserPolicyInput{
			UserName: aws.String(user), PolicyName: aws.String("grant"), PolicyDocument: aws.String(string(doc)),
		})
		require.NoError(t, err, "put-user-policy %s", user)
		t.Cleanup(func() {
			if _, err := c.DeleteUserPolicy(&iam.DeleteUserPolicyInput{UserName: aws.String(user), PolicyName: aws.String("grant")}); err != nil {
				t.Logf("cleanup: delete-user-policy %s: %v", user, err)
			}
		})
	}

	key, err := c.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(user)})
	require.NoError(t, err, "create-access-key %s", user)
	t.Cleanup(func() {
		if _, err := c.DeleteAccessKey(&iam.DeleteAccessKeyInput{UserName: aws.String(user), AccessKeyId: key.AccessKey.AccessKeyId}); err != nil {
			t.Logf("cleanup: delete-access-key %s: %v", user, err)
		}
	})
	id, secret := aws.StringValue(key.AccessKey.AccessKeyId), aws.StringValue(key.AccessKey.SecretAccessKey)
	client := harness.NewAWSClientWithCreds(t, env, id, secret)

	// A denial proves the key authenticates; anything else is a key not yet live.
	harness.EventuallyErr(t, func() error {
		_, err := client.Route53.ListHostedZones(&r53.ListHostedZonesInput{})
		if err == nil || harness.ErrorCodeIs(err, "AccessDenied") {
			return nil
		}
		return fmt.Errorf("credentials for %s are not live yet: %w", user, err)
	}, 60*time.Second, 2*time.Second)
	return principal{AWSClient: client, userName: user, key: id, secret: secret}
}
