//go:build e2e

package single

import (
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
)

// IAM state shared across the IAM phase tests. Each phase test is its own
// top-level Test*, so cross-phase state (users, primary access keys, and the
// derived admin account ID) is memoized at package scope behind sync.Once.
// LIFO cleanup is registered against fix.Harness so it runs at fixture
// teardown rather than at end of the first caller's subtest.
//
// All helpers tolerate "already exists" outcomes so a caller that races a
// phase test which is itself exercising create-then-delete (e.g. IAM1) still
// converges on a populated state.

var (
	iamAliceOnce   sync.Once
	iamAliceErr    error
	iamBobOnce     sync.Once
	iamBobErr      error
	iamCharlieOnce sync.Once
	iamCharlieErr  error

	iamAliceKeyOnce   sync.Once
	iamAliceKeyID     string
	iamAliceKeySecret string
	iamAliceKeyErr    error

	iamBobKeyOnce   sync.Once
	iamBobKeyID     string
	iamBobKeySecret string
	iamBobKeyErr    error

	iamCharlieKeyOnce   sync.Once
	iamCharlieKeyID     string
	iamCharlieKeySecret string
	iamCharlieKeyErr    error

	iamAdminAccountOnce sync.Once
	iamAdminAccountID   string
	iamAdminAccountErr  error
)

// iamEnsureAlice ensures the canonical "alice" IAM user exists. The user is
// torn down at fixture teardown via iamDeleteUserBestEffort.
func iamEnsureAlice(t *testing.T, fix *Fixture) {
	t.Helper()
	iamAliceOnce.Do(func() {
		iamAliceErr = iamCreateUserIdempotent(fix, iamUserAlice, "")
		if iamAliceErr == nil {
			fix.Harness.RegisterCleanup(func() { iamDeleteUserBestEffort(fix, iamUserAlice) })
		}
	})
	if iamAliceErr != nil {
		t.Fatalf("iamEnsureAlice: %v", iamAliceErr)
	}
}

// iamEnsureBob ensures the canonical "bob" IAM user (path /engineering/)
// exists. Torn down at fixture teardown.
func iamEnsureBob(t *testing.T, fix *Fixture) {
	t.Helper()
	iamBobOnce.Do(func() {
		iamBobErr = iamCreateUserIdempotent(fix, iamUserBob, iamUserBobPath)
		if iamBobErr == nil {
			fix.Harness.RegisterCleanup(func() { iamDeleteUserBestEffort(fix, iamUserBob) })
		}
	})
	if iamBobErr != nil {
		t.Fatalf("iamEnsureBob: %v", iamBobErr)
	}
}

// iamEnsureCharlie ensures the canonical "charlie" IAM user exists. Torn down
// at fixture teardown.
func iamEnsureCharlie(t *testing.T, fix *Fixture) {
	t.Helper()
	iamCharlieOnce.Do(func() {
		iamCharlieErr = iamCreateUserIdempotent(fix, iamUserCharlie, "")
		if iamCharlieErr == nil {
			fix.Harness.RegisterCleanup(func() { iamDeleteUserBestEffort(fix, iamUserCharlie) })
		}
	})
	if iamCharlieErr != nil {
		t.Fatalf("iamEnsureCharlie: %v", iamCharlieErr)
	}
}

// iamEnsureAliceKey returns alice's primary access key + secret, creating
// the key (and alice) on first call.
func iamEnsureAliceKey(t *testing.T, fix *Fixture) (keyID, secret string) {
	t.Helper()
	iamEnsureAlice(t, fix)
	iamAliceKeyOnce.Do(func() {
		iamAliceKeyID, iamAliceKeySecret, iamAliceKeyErr = iamCreateAccessKey(fix, iamUserAlice)
	})
	if iamAliceKeyErr != nil {
		t.Fatalf("iamEnsureAliceKey: %v", iamAliceKeyErr)
	}
	return iamAliceKeyID, iamAliceKeySecret
}

// iamEnsureBobKey returns bob's primary access key + secret, creating the
// key (and bob) on first call.
func iamEnsureBobKey(t *testing.T, fix *Fixture) (keyID, secret string) {
	t.Helper()
	iamEnsureBob(t, fix)
	iamBobKeyOnce.Do(func() {
		iamBobKeyID, iamBobKeySecret, iamBobKeyErr = iamCreateAccessKey(fix, iamUserBob)
	})
	if iamBobKeyErr != nil {
		t.Fatalf("iamEnsureBobKey: %v", iamBobKeyErr)
	}
	return iamBobKeyID, iamBobKeySecret
}

// iamEnsureCharlieKey returns charlie's primary access key + secret,
// creating the key (and charlie) on first call.
func iamEnsureCharlieKey(t *testing.T, fix *Fixture) (keyID, secret string) {
	t.Helper()
	iamEnsureCharlie(t, fix)
	iamCharlieKeyOnce.Do(func() {
		iamCharlieKeyID, iamCharlieKeySecret, iamCharlieKeyErr = iamCreateAccessKey(fix, iamUserCharlie)
	})
	if iamCharlieKeyErr != nil {
		t.Fatalf("iamEnsureCharlieKey: %v", iamCharlieKeyErr)
	}
	return iamCharlieKeyID, iamCharlieKeySecret
}

// iamEnsureAdminAccountID returns the active account ID, derived from the
// ARN of a probe IAM resource. Uses GetUser on the canonical alice user so
// no policy churn is needed; alice is created on first call.
func iamEnsureAdminAccountID(t *testing.T, fix *Fixture) string {
	t.Helper()
	iamEnsureAlice(t, fix)
	iamAdminAccountOnce.Do(func() {
		out, err := fix.AWS.IAM.GetUser(&iam.GetUserInput{UserName: aws.String(iamUserAlice)})
		if err != nil {
			iamAdminAccountErr = err
			return
		}
		iamAdminAccountID = iamAccountFromARN(t, aws.StringValue(out.User.Arn))
	})
	if iamAdminAccountErr != nil {
		t.Fatalf("iamEnsureAdminAccountID: %v", iamAdminAccountErr)
	}
	return iamAdminAccountID
}

// iamCreateUserIdempotent creates a user, swallowing EntityAlreadyExists so
// the helper can run after a phase test that already created the user.
func iamCreateUserIdempotent(fix *Fixture, name, path string) error {
	input := &iam.CreateUserInput{UserName: aws.String(name)}
	if path != "" {
		input.Path = aws.String(path)
	}
	_, err := fix.AWS.IAM.CreateUser(input)
	if err == nil {
		return nil
	}
	// EntityAlreadyExists: user came from a parallel phase test. Treat as
	// success — we just need the named user to exist.
	if iamIsEntityAlreadyExists(err) {
		return nil
	}
	return err
}

// iamDeleteAllKeys drops every access key for user. Used as a "clean slate"
// before tests that assert against the AWS 2-key per-user cap.
func iamDeleteAllKeys(fix *Fixture, user string) {
	keys, err := fix.AWS.IAM.ListAccessKeys(&iam.ListAccessKeysInput{
		UserName: aws.String(user),
	})
	if err != nil {
		return
	}
	for _, k := range keys.AccessKeyMetadata {
		_, _ = fix.AWS.IAM.DeleteAccessKey(&iam.DeleteAccessKeyInput{
			UserName:    aws.String(user),
			AccessKeyId: k.AccessKeyId,
		})
	}
}

// iamCreateAccessKey creates a fresh access key on user. Returns the
// (id, secret) pair plus any error surfaced by the API.
func iamCreateAccessKey(fix *Fixture, user string) (string, string, error) {
	out, err := fix.AWS.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{
		UserName: aws.String(user),
	})
	if err != nil {
		return "", "", err
	}
	return aws.StringValue(out.AccessKey.AccessKeyId),
		aws.StringValue(out.AccessKey.SecretAccessKey),
		nil
}

// iamIsEntityAlreadyExists reports whether err is an AWS EntityAlreadyExists
// surface, used by idempotent create helpers.
func iamIsEntityAlreadyExists(err error) bool {
	type coder interface{ Code() string }
	if c, ok := err.(coder); ok {
		return c.Code() == "EntityAlreadyExists"
	}
	return false
}
