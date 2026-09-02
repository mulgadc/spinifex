//go:build e2e

package iam

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// The address range the "somewhere else" aws:SourceIp case grants from. It is
// reserved for documentation, so no runner can legitimately be inside it.
const authzForeignCIDR = "203.0.113.0/24"

// runIAMAuthorization drives identity-policy enforcement against a live stack:
// the resolution path every subtest exercises runs over NATS-backed IAM state,
// which the in-process tiers substitute out.
//
// Key pairs are the resource under test because ec2:CreateKeyPair is scoped by
// the caller-supplied KeyName, so a grant can name one key pair and a sibling
// request is denied without booting anything.
func runIAMAuthorization(t *testing.T, fix *Fixture) {
	account := harness.IAMAccountID(t, fix.AWS)
	run := fmt.Sprintf("iam-authz-%d", time.Now().UnixNano())

	t.Run("AResourceScopedGrantNamesOneKeyPair", func(t *testing.T) {
		granted, sibling := run+"-granted", run+"-sibling"
		principal := newAuthzPrincipal(t, fix, "scoped", authzStatement{
			Effect:   "Allow",
			Action:   []string{"ec2:CreateKeyPair"},
			Resource: []string{keyPairARN(account, granted)},
		})

		createKeyPair(t, fix, principal, granted)

		harness.ExpectError(t, "AccessDenied", func() error {
			_, err := principal.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(sibling)}) // e2e:allow-create — the create is the authorization probe
			return err
		})
		// The denial has to land ahead of dispatch, or a refused create still
		// consumed the name.
		harness.ExpectError(t, "InvalidKeyPair.NotFound", func() error {
			_, err := fix.AWS.EC2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
				KeyNames: []*string{aws.String(sibling)},
			})
			return err
		})
	})

	t.Run("ADenyAppliesToTheNextRequestWithoutANewCredential", func(t *testing.T) {
		principal := newAuthzPrincipal(t, fix, "deny", authzStatement{
			Effect:   "Allow",
			Action:   []string{"ec2:CreateKeyPair"},
			Resource: []string{keyPairARN(account, run+"-deny-*")},
		})
		createKeyPair(t, fix, principal, run+"-deny-1")

		denyARN := newManagedPolicy(t, fix, "deny", authzStatement{
			Effect:   "Deny",
			Action:   []string{"ec2:CreateKeyPair"},
			Resource: []string{"*"},
		})
		_, err := fix.AWS.IAM.AttachUserPolicy(&iam.AttachUserPolicyInput{
			UserName: aws.String(principal.UserName), PolicyArn: aws.String(denyARN),
		})
		require.NoError(t, err, "attach-user-policy %s", denyARN)

		// Same client, same key: the credential is a pointer into live IAM state,
		// not a capability minted when the key was issued.
		harness.ExpectError(t, "AccessDenied", func() error {
			_, err := principal.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{ // e2e:allow-create — the create is the authorization probe
				KeyName: aws.String(run + "-deny-2"),
			})
			return err
		})

		_, err = fix.AWS.IAM.DetachUserPolicy(&iam.DetachUserPolicyInput{
			UserName: aws.String(principal.UserName), PolicyArn: aws.String(denyARN),
		})
		require.NoError(t, err, "detach-user-policy %s", denyARN)
		createKeyPair(t, fix, principal, run+"-deny-3")
	})

	t.Run("DeletingTheGrantingPolicyRevokesTheNextRequest", func(t *testing.T) {
		principal := newAuthzPrincipal(t, fix, "attached")
		grantARN := newManagedPolicy(t, fix, "attached", authzStatement{
			Effect:   "Allow",
			Action:   []string{"ec2:CreateKeyPair"},
			Resource: []string{keyPairARN(account, run+"-attached-*")},
		})
		_, err := fix.AWS.IAM.AttachUserPolicy(&iam.AttachUserPolicyInput{
			UserName: aws.String(principal.UserName), PolicyArn: aws.String(grantARN),
		})
		require.NoError(t, err, "attach-user-policy %s", grantARN)

		// The attach is the principal's only grant, so it has to be live before
		// its removal proves anything.
		harness.EventuallyErr(t, func() error {
			_, err := principal.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{ // e2e:allow-create — the create is the authorization probe
				KeyName: aws.String(run + "-attached-1"),
			})
			return err
		}, 30*time.Second, 2*time.Second)
		t.Cleanup(func() { deleteKeyPairBestEffort(t, fix, run+"-attached-1") })

		_, err = fix.AWS.IAM.DetachUserPolicy(&iam.DetachUserPolicyInput{
			UserName: aws.String(principal.UserName), PolicyArn: aws.String(grantARN),
		})
		require.NoError(t, err, "detach-user-policy %s", grantARN)
		_, err = fix.AWS.IAM.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(grantARN)})
		require.NoError(t, err, "delete-policy %s", grantARN)

		harness.ExpectError(t, "AccessDenied", func() error {
			_, err := principal.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{ // e2e:allow-create — the create is the authorization probe
				KeyName: aws.String(run + "-attached-2"),
			})
			return err
		})
	})

	t.Run("RevokingACredentialStopsThatKeyAlone", func(t *testing.T) {
		principal := newAuthzPrincipal(t, fix, "revoke", authzStatement{
			Effect:   "Allow",
			Action:   []string{"ec2:CreateKeyPair"},
			Resource: []string{keyPairARN(account, run+"-revoke-*")},
		})
		second := addAccessKey(t, fix, principal)

		_, err := fix.AWS.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{
			UserName: aws.String(principal.UserName), AccessKeyId: aws.String(principal.KeyID),
		})
		require.NoError(t, err, "delete-access-key %s", principal.KeyID)

		// Not AccessDenied: a deleted key no longer authenticates, so the request
		// never reaches policy evaluation.
		harness.ExpectError(t, "InvalidClientTokenId", func() error {
			_, err := principal.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{ // e2e:allow-create — the create is the authorization probe
				KeyName: aws.String(run + "-revoke-1"),
			})
			return err
		})
		createKeyPair(t, fix, second, run+"-revoke-2")

		// A user holding keys cannot be deleted, so deleting the user is observed
		// as the tail of this sequence: no credential of a deleted user survives it.
		_, err = fix.AWS.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{
			UserName: aws.String(principal.UserName), AccessKeyId: aws.String(second.KeyID),
		})
		require.NoError(t, err, "delete-access-key %s", second.KeyID)
		deleteAuthzUser(t, fix, principal.UserName)

		harness.ExpectError(t, "InvalidClientTokenId", func() error {
			_, err := second.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{ // e2e:allow-create — the create is the authorization probe
				KeyName: aws.String(run + "-revoke-3"),
			})
			return err
		})
		harness.ExpectError(t, "NoSuchEntity", func() error {
			_, err := fix.AWS.IAM.GetUser(&iam.GetUserInput{UserName: aws.String(principal.UserName)})
			return err
		})
	})

	t.Run("AnAssumedRoleSessionCarriesTheRolePolicies", func(t *testing.T) {
		roleName := uniqueAuthzName("role")
		roleARN := harness.IAMRoleARN(account, roleName)
		createAuthzRole(t, fix, roleName, authzStatement{
			Effect:   "Allow",
			Action:   []string{"ec2:CreateKeyPair"},
			Resource: []string{keyPairARN(account, run+"-role-*")},
		})

		principal := newAuthzPrincipal(t, fix, "assume",
			authzStatement{
				Effect:   "Allow",
				Action:   []string{"ec2:CreateKeyPair"},
				Resource: []string{keyPairARN(account, run+"-user-*")},
			},
			authzStatement{
				Effect:   "Allow",
				Action:   []string{"sts:AssumeRole"},
				Resource: []string{roleARN},
			})

		createKeyPair(t, fix, principal, run+"-user-1")
		harness.ExpectError(t, "AccessDenied", func() error {
			_, err := principal.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{ // e2e:allow-create — the create is the authorization probe
				KeyName: aws.String(run + "-role-1"),
			})
			return err
		})

		assumed, err := principal.Client.STS.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(roleARN),
			RoleSessionName: aws.String(uniqueAuthzName("session")),
		})
		require.NoError(t, err, "assume-role %s", roleARN)
		require.NotNil(t, assumed.Credentials, "assume-role returned no credentials")
		session := harness.NewAWSClientWithSessionCreds(t, fix.Env,
			aws.StringValue(assumed.Credentials.AccessKeyId),
			aws.StringValue(assumed.Credentials.SecretAccessKey),
			aws.StringValue(assumed.Credentials.SessionToken))

		createKeyPair(t, fix, &authzPrincipal{Client: session}, run+"-role-2")
		// The grant that carried the assuming user does not follow the session:
		// the role's policies are the whole of what it holds.
		harness.ExpectError(t, "AccessDenied", func() error {
			_, err := session.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{ // e2e:allow-create — the create is the authorization probe
				KeyName: aws.String(run + "-user-2"),
			})
			return err
		})
	})

	t.Run("ASourceIPConditionSeesTheRealClientAddress", func(t *testing.T) {
		here := observedClientCIDR(t, fix.Env)

		granted := newAuthzPrincipal(t, fix, "sourceip-here", authzStatement{
			Effect:    "Allow",
			Action:    []string{"ec2:CreateKeyPair"},
			Resource:  []string{keyPairARN(account, run+"-ip-*")},
			Condition: sourceIPCondition(here),
		})
		createKeyPair(t, fix, granted, run+"-ip-1")

		elsewhere := newAuthzPrincipal(t, fix, "sourceip-elsewhere", authzStatement{
			Effect:    "Allow",
			Action:    []string{"ec2:CreateKeyPair"},
			Resource:  []string{keyPairARN(account, run+"-ip-*")},
			Condition: sourceIPCondition(authzForeignCIDR),
		})
		harness.ExpectError(t, "AccessDenied", func() error {
			_, err := elsewhere.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{ // e2e:allow-create — the create is the authorization probe
				KeyName: aws.String(run + "-ip-2"),
			})
			return err
		})

		// The Deny arm: an unconditional Allow alongside it, so a Deny that failed
		// to fire is a create that succeeds rather than a missing grant.
		fenced := newAuthzPrincipal(t, fix, "sourceip-deny",
			authzStatement{
				Effect:   "Allow",
				Action:   []string{"ec2:CreateKeyPair"},
				Resource: []string{keyPairARN(account, run+"-ip-*")},
			},
			authzStatement{
				Effect:    "Deny",
				Action:    []string{"ec2:CreateKeyPair"},
				Resource:  []string{"*"},
				Condition: sourceIPCondition(here),
			})
		harness.ExpectError(t, "AccessDenied", func() error {
			_, err := fenced.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{ // e2e:allow-create — the create is the authorization probe
				KeyName: aws.String(run + "-ip-3"),
			})
			return err
		})
	})
}

// One statement of a test principal's policy document.
type authzStatement struct {
	Effect    string                         `json:"Effect"`
	Action    []string                       `json:"Action"`
	Resource  []string                       `json:"Resource"`
	Condition map[string]map[string][]string `json:"Condition,omitempty"`
}

// A test user plus a client signing as it. Client alone is populated for the
// session and second-key clients, which own no user of their own.
type authzPrincipal struct {
	UserName string
	KeyID    string
	Client   *harness.AWSClient
}

// newAuthzPrincipal creates a user carrying exactly statements and nothing else,
// with one access key, and returns a client signing as it. The suite's own
// credentials are an administrator, so every denial needs a narrower principal.
func newAuthzPrincipal(t *testing.T, fix *Fixture, name string, statements ...authzStatement) *authzPrincipal {
	t.Helper()
	userName := uniqueAuthzName(name)

	_, err := fix.AWS.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "create-user %s", userName)
	t.Cleanup(func() { deleteAuthzUserBestEffort(t, fix, userName) })

	const policyName = "iam-e2e-grant"
	if len(statements) > 0 {
		_, err = fix.AWS.IAM.PutUserPolicy(&iam.PutUserPolicyInput{
			UserName:       aws.String(userName),
			PolicyName:     aws.String(policyName),
			PolicyDocument: aws.String(authzDocument(t, statements...)),
		})
		require.NoError(t, err, "put-user-policy %s", userName)
	}

	principal := &authzPrincipal{UserName: userName}
	key := addAccessKey(t, fix, principal)
	principal.KeyID, principal.Client = key.KeyID, key.Client
	return principal
}

// addAccessKey mints an additional key for principal's user and returns a client
// signing with it, waiting for the key to authenticate: a credential that is not
// live yet fails differently from one that is denied.
func addAccessKey(t *testing.T, fix *Fixture, principal *authzPrincipal) *authzPrincipal {
	t.Helper()
	out, err := fix.AWS.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{
		UserName: aws.String(principal.UserName),
	})
	require.NoError(t, err, "create-access-key %s", principal.UserName)
	keyID := aws.StringValue(out.AccessKey.AccessKeyId)
	t.Cleanup(func() {
		if _, err := fix.AWS.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{
			UserName: aws.String(principal.UserName), AccessKeyId: aws.String(keyID),
		}); err != nil {
			t.Logf("cleanup: delete-access-key %s: %v", keyID, err)
		}
	})

	client := harness.NewAWSClientWithCreds(t, fix.Env, keyID, aws.StringValue(out.AccessKey.SecretAccessKey))
	harness.EventuallyErr(t, func() error {
		_, err := client.EC2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{})
		if err == nil || harness.ErrorCodeIs(err, "AccessDenied") {
			return nil
		}
		return fmt.Errorf("credentials for %s are not live yet: %w", principal.UserName, err)
	}, 60*time.Second, 2*time.Second)
	return &authzPrincipal{UserName: principal.UserName, KeyID: keyID, Client: client}
}

// newManagedPolicy creates a customer-managed policy and returns its ARN. The
// caller owns the delete where the test asserts on it; otherwise cleanup does.
func newManagedPolicy(t *testing.T, fix *Fixture, name string, statements ...authzStatement) string {
	t.Helper()
	policyName := uniqueAuthzName(name)
	out, err := fix.AWS.IAM.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(authzDocument(t, statements...)),
	})
	require.NoError(t, err, "create-policy %s", policyName)
	arn := aws.StringValue(out.Policy.Arn)
	t.Cleanup(func() {
		if _, err := fix.AWS.IAM.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(arn)}); err != nil {
			t.Logf("cleanup: delete-policy %s: %v", arn, err)
		}
	})
	return arn
}

// createAuthzRole creates a role assumable by any principal in the account,
// carrying statements as its only inline policy. The trust policy is wide on
// purpose: what is under test is the identity policy the session resolves.
func createAuthzRole(t *testing.T, fix *Fixture, roleName string, statements ...authzStatement) {
	t.Helper()
	const trustPolicy = `{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`

	_, err := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
	})
	require.NoError(t, err, "create-role %s", roleName)

	const policyName = "iam-e2e-role-grant"
	_, err = fix.AWS.IAM.PutRolePolicy(&iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(authzDocument(t, statements...)),
	})
	require.NoError(t, err, "put-role-policy %s", roleName)

	t.Cleanup(func() {
		if _, err := fix.AWS.IAM.DeleteRolePolicy(&iam.DeleteRolePolicyInput{
			RoleName: aws.String(roleName), PolicyName: aws.String(policyName),
		}); err != nil {
			t.Logf("cleanup: delete-role-policy %s: %v", roleName, err)
		}
		harness.IAMDeleteRoleAndProfilesBestEffort(fix.AWS, roleName, nil)
	})
}

// createKeyPair asserts principal may create name, and registers its delete.
func createKeyPair(t *testing.T, fix *Fixture, principal *authzPrincipal, name string) {
	t.Helper()
	_, err := principal.Client.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(name)}) // e2e:allow-create — the create is the authorization probe
	require.NoError(t, err, "create-key-pair %s must be allowed by the principal's grant", name)
	t.Cleanup(func() { deleteKeyPairBestEffort(t, fix, name) })
}

// deleteKeyPairBestEffort removes a key pair as the suite administrator.
func deleteKeyPairBestEffort(t *testing.T, fix *Fixture, name string) {
	t.Helper()
	if _, err := fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(name)}); err != nil {
		t.Logf("cleanup: delete-key-pair %s: %v", name, err)
	}
}

// deleteAuthzUser drops a user's inline policies and attachments, then the user
// itself, failing the test on error: a subtest that deletes a user asserts on it.
func deleteAuthzUser(t *testing.T, fix *Fixture, userName string) {
	t.Helper()
	require.NoError(t, detachAuthzUser(fix, userName), "detach policies from %s", userName)
	_, err := fix.AWS.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(userName)})
	require.NoError(t, err, "delete-user %s", userName)
}

// deleteAuthzUserBestEffort is the cleanup form, tolerating a user a subtest
// already deleted or never fully created.
func deleteAuthzUserBestEffort(t *testing.T, fix *Fixture, userName string) {
	t.Helper()
	if err := detachAuthzUser(fix, userName); err != nil {
		t.Logf("cleanup: detach policies from %s: %v", userName, err)
	}
	if _, err := fix.AWS.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(userName)}); err != nil {
		t.Logf("cleanup: delete-user %s: %v", userName, err)
	}
}

// detachAuthzUser empties a user of the attachments that block its delete. Keys
// are owned by the cleanup addAccessKey registers, so they are not touched here.
func detachAuthzUser(fix *Fixture, userName string) error {
	inline, err := fix.AWS.IAM.ListUserPolicies(&iam.ListUserPoliciesInput{UserName: aws.String(userName)})
	if err != nil {
		return fmt.Errorf("list-user-policies %s: %w", userName, err)
	}
	for _, name := range inline.PolicyNames {
		if _, err := fix.AWS.IAM.DeleteUserPolicy(&iam.DeleteUserPolicyInput{
			UserName: aws.String(userName), PolicyName: name,
		}); err != nil {
			return fmt.Errorf("delete-user-policy %s/%s: %w", userName, aws.StringValue(name), err)
		}
	}
	attached, err := fix.AWS.IAM.ListAttachedUserPolicies(&iam.ListAttachedUserPoliciesInput{
		UserName: aws.String(userName),
	})
	if err != nil {
		return fmt.Errorf("list-attached-user-policies %s: %w", userName, err)
	}
	for _, p := range attached.AttachedPolicies {
		if _, err := fix.AWS.IAM.DetachUserPolicy(&iam.DetachUserPolicyInput{
			UserName: aws.String(userName), PolicyArn: p.PolicyArn,
		}); err != nil {
			return fmt.Errorf("detach-user-policy %s/%s: %w", userName, aws.StringValue(p.PolicyArn), err)
		}
	}
	return nil
}

// authzDocument renders statements as a policy document.
func authzDocument(t *testing.T, statements ...authzStatement) string {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"Version":   "2012-10-17",
		"Statement": statements,
	})
	require.NoError(t, err, "marshal policy document")
	return string(document)
}

// sourceIPCondition builds the one condition block the gateway resolves from the
// request itself rather than from the principal.
func sourceIPCondition(cidr string) map[string]map[string][]string {
	return map[string]map[string][]string{"IpAddress": {"aws:SourceIp": {cidr}}}
}

// keyPairARN names one key pair, or a prefix of them, in any region: the suite
// asserts on the resource segment, and the gateway anchors the region itself.
func keyPairARN(account, name string) string {
	return "arn:aws:ec2:*:" + account + ":key-pair/" + name
}

// uniqueAuthzName suffixes a name so a failed run's leftovers cannot be mistaken
// for this run's principal, which would assert against the wrong grant.
func uniqueAuthzName(name string) string {
	return fmt.Sprintf("iam-e2e-%s-%d", name, time.Now().UnixNano())
}

// observedClientCIDR returns the host-sized prefix of the address this process
// reaches the gateway from, read off a real connection to it. A hardcoded
// loopback would pass on a runner the gateway never sees as one.
func observedClientCIDR(t *testing.T, env *harness.Env) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", gatewayHostPort(t, env), 10*time.Second)
	require.NoError(t, err, "dial the gateway to observe this client's address")
	defer func() { _ = conn.Close() }()

	local, err := netip.ParseAddrPort(conn.LocalAddr().String())
	require.NoError(t, err, "parse local address %q", conn.LocalAddr())
	addr := local.Addr().Unmap()

	bits := addr.BitLen()
	prefix := netip.PrefixFrom(addr, bits)
	require.False(t, prefix.Contains(netip.MustParseAddr("203.0.113.1")),
		"this runner sits inside %s, which the foreign-address case assumes is unreachable", authzForeignCIDR)
	return prefix.String()
}

// gatewayHostPort resolves the address the SDK clients dial, mirroring the
// harness's own endpoint resolution.
func gatewayHostPort(t *testing.T, env *harness.Env) string {
	t.Helper()
	endpoint := os.Getenv("SPINIFEX_AWS_ENDPOINT")
	if endpoint == "" {
		host := "127.0.0.1"
		if len(env.ServiceIPs) > 0 {
			host = env.ServiceIPs[0]
		}
		return net.JoinHostPort(host, strconv.Itoa(env.AWSGWPort))
	}
	u, err := url.Parse(endpoint)
	require.NoError(t, err, "parse SPINIFEX_AWS_ENDPOINT %q", endpoint)
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}
