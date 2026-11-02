package iam

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// A user looping a self-scoped write must never undo an administrator's
// DetachUserPolicy.
func TestTagUserLoop_DoesNotUndoDetachUserPolicy(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	user := createTestUser(t, svc, "self-tagger")
	policy := createTestPolicy(t, svc, "RevokedPolicy")

	for trial := range 40 {
		_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{UserName: user.UserName, PolicyArn: policy.Arn})
		require.NoError(t, err)

		var stop atomic.Bool
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				for !stop.Load() {
					_, _ = svc.TagUser(testAccountID, &iam.TagUserInput{
						UserName: user.UserName,
						Tags:     []*iam.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
					})
				}
			})
		}

		_, err = svc.DetachUserPolicy(testAccountID, &iam.DetachUserPolicyInput{UserName: user.UserName, PolicyArn: policy.Arn})
		stop.Store(true)
		wg.Wait()
		require.NoError(t, err)

		got, err := svc.getUser(t.Context(), testAccountID, *user.UserName)
		require.NoError(t, err)
		require.Empty(t, got.AttachedPolicies, "trial %d: a concurrent TagUser reattached the detached policy", trial)
	}
}

// Every user writer that read the record before a DetachUserPolicy and writes
// after it must leave the policy detached and still apply its own change.
func TestUserWriters_DoNotUndoConcurrentDetach(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, svc *IAMServiceImpl, userName string) string
		write func(svc *IAMServiceImpl, userName, arg string) error
		check func(t *testing.T, u *User)
	}{
		{"TagUser", nil, func(svc *IAMServiceImpl, userName, _ string) error {
			_, err := svc.TagUser(testAccountID, &iam.TagUserInput{
				UserName: aws.String(userName),
				Tags:     []*iam.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
			})
			return err
		}, func(t *testing.T, u *User) { assert.Len(t, u.Tags, 1) }},
		{"UntagUser", nil, func(svc *IAMServiceImpl, userName, _ string) error {
			_, err := svc.UntagUser(testAccountID, &iam.UntagUserInput{
				UserName: aws.String(userName),
				TagKeys:  aws.StringSlice([]string{"absent"}),
			})
			return err
		}, nil},
		{"CreateAccessKey", nil, func(svc *IAMServiceImpl, userName, _ string) error {
			_, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{UserName: aws.String(userName)})
			return err
		}, func(t *testing.T, u *User) { assert.Len(t, u.AccessKeys, 1) }},
		{"DeleteAccessKey", func(t *testing.T, svc *IAMServiceImpl, userName string) string {
			out, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{UserName: aws.String(userName)})
			require.NoError(t, err)
			return *out.AccessKey.AccessKeyId
		}, func(svc *IAMServiceImpl, userName, accessKeyID string) error {
			_, err := svc.DeleteAccessKey(testAccountID, &iam.DeleteAccessKeyInput{
				UserName:    aws.String(userName),
				AccessKeyId: aws.String(accessKeyID),
			})
			return err
		}, func(t *testing.T, u *User) { assert.Empty(t, u.AccessKeys) }},
		{"AttachUserPolicy", func(t *testing.T, svc *IAMServiceImpl, _ string) string {
			return *createTestPolicy(t, svc, "OtherPolicy").Arn
		}, func(svc *IAMServiceImpl, userName, policyARN string) error {
			_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
				UserName:  aws.String(userName),
				PolicyArn: aws.String(policyARN),
			})
			return err
		}, func(t *testing.T, u *User) { assert.Len(t, u.AttachedPolicies, 1) }},
		{"DeleteUserPolicy", func(t *testing.T, svc *IAMServiceImpl, userName string) string {
			_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
				UserName:       aws.String(userName),
				PolicyName:     aws.String("seeded"),
				PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`),
			})
			require.NoError(t, err)
			return ""
		}, func(svc *IAMServiceImpl, userName, _ string) error {
			_, err := svc.DeleteUserPolicy(testAccountID, &iam.DeleteUserPolicyInput{
				UserName:   aws.String(userName),
				PolicyName: aws.String("seeded"),
			})
			return err
		}, func(t *testing.T, u *User) { assert.Empty(t, u.InlinePolicies) }},
		{"RemoveUserFromGroup", func(t *testing.T, svc *IAMServiceImpl, userName string) string {
			createTestGroup(t, svc, "devs")
			_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
				UserName:  aws.String(userName),
				GroupName: aws.String("devs"),
			})
			require.NoError(t, err)
			return ""
		}, func(svc *IAMServiceImpl, userName, _ string) error {
			_, err := svc.RemoveUserFromGroup(testAccountID, &iam.RemoveUserFromGroupInput{
				UserName:  aws.String(userName),
				GroupName: aws.String("devs"),
			})
			return err
		}, func(t *testing.T, u *User) { assert.Empty(t, u.Groups) }},
		{"PutUserPolicy", nil, func(svc *IAMServiceImpl, userName, _ string) error {
			_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
				UserName:       aws.String(userName),
				PolicyName:     aws.String("inline"),
				PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`),
			})
			return err
		}, func(t *testing.T, u *User) { assert.Contains(t, u.InlinePolicies, "inline") }},
		{"AddUserToGroup", func(t *testing.T, svc *IAMServiceImpl, _ string) string {
			createTestGroup(t, svc, "devs")
			return ""
		}, func(svc *IAMServiceImpl, userName, _ string) error {
			_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
				UserName:  aws.String(userName),
				GroupName: aws.String("devs"),
			})
			return err
		}, func(t *testing.T, u *User) { assert.Equal(t, []string{"devs"}, u.Groups) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			user := createTestUser(t, svc, "detach-race")
			policy := createTestPolicy(t, svc, "DetachRacePolicy")
			_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{UserName: user.UserName, PolicyArn: policy.Arn})
			require.NoError(t, err)
			var arg string
			if tc.setup != nil {
				arg = tc.setup(t, svc, *user.UserName)
			}

			gate := &gatedKV{
				KeyValue: svc.usersBucket,
				key:      testAccountID + "." + *user.UserName,
				reached:  make(chan struct{}),
				release:  make(chan struct{}),
			}
			gate.armed.Store(true)
			svc.usersBucket = gate

			done := make(chan error, 1)
			go func() { done <- tc.write(svc, *user.UserName, arg) }()
			<-gate.reached

			_, err = peerIAMService(t, svc).DetachUserPolicy(testAccountID, &iam.DetachUserPolicyInput{UserName: user.UserName, PolicyArn: policy.Arn})
			require.NoError(t, err)

			close(gate.release)
			require.NoError(t, <-done)

			got, err := svc.getUser(t.Context(), testAccountID, *user.UserName)
			require.NoError(t, err)
			assert.NotContains(t, got.AttachedPolicies, *policy.Arn, "the write must not reattach the detached policy")
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// A user write racing DeleteUser must fail NoSuchEntity, not recreate the user.
func TestTagUser_DoesNotRecreateConcurrentlyDeletedUser(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	user := createTestUser(t, svc, "deleted-mid-write")

	gate := &gatedKV{
		KeyValue: svc.usersBucket,
		key:      testAccountID + "." + *user.UserName,
		reached:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	gate.armed.Store(true)
	svc.usersBucket = gate

	done := make(chan error, 1)
	go func() {
		_, err := svc.TagUser(testAccountID, &iam.TagUserInput{
			UserName: user.UserName,
			Tags:     []*iam.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
		})
		done <- err
	}()
	<-gate.reached

	_, err := svc.DeleteUser(testAccountID, &iam.DeleteUserInput{UserName: user.UserName})
	require.NoError(t, err)

	close(gate.release)
	err = <-done
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())

	_, err = svc.getUser(t.Context(), testAccountID, *user.UserName)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error(), "the deleted user must stay deleted")
}

// Every group writer that read the record before a DetachGroupPolicy and writes
// after it must leave the policy detached.
func TestGroupWriters_DoNotUndoConcurrentDetach(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		write func(svc *IAMServiceImpl, groupName, otherPolicyARN string) error
	}{
		{"AttachGroupPolicy", func(svc *IAMServiceImpl, groupName, otherPolicyARN string) error {
			_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
				GroupName: aws.String(groupName),
				PolicyArn: aws.String(otherPolicyARN),
			})
			return err
		}},
		{"PutGroupPolicy", func(svc *IAMServiceImpl, groupName, _ string) error {
			_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
				GroupName:      aws.String(groupName),
				PolicyName:     aws.String("inline"),
				PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`),
			})
			return err
		}},
		{"DeleteGroupPolicy", func(svc *IAMServiceImpl, groupName, _ string) error {
			_, err := svc.DeleteGroupPolicy(testAccountID, &iam.DeleteGroupPolicyInput{
				GroupName:  aws.String(groupName),
				PolicyName: aws.String("seeded"),
			})
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			group := createTestGroup(t, svc, "detach-race")
			policy := createTestPolicy(t, svc, "DetachRacePolicy")
			other := createTestPolicy(t, svc, "OtherPolicy")
			_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{GroupName: group.GroupName, PolicyArn: policy.Arn})
			require.NoError(t, err)
			_, err = svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
				GroupName:      group.GroupName,
				PolicyName:     aws.String("seeded"),
				PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:ListBucket","Resource":"*"}]}`),
			})
			require.NoError(t, err)

			gate := &gatedKV{
				KeyValue: svc.groupsBucket,
				key:      testAccountID + "." + *group.GroupName,
				reached:  make(chan struct{}),
				release:  make(chan struct{}),
			}
			gate.armed.Store(true)
			svc.groupsBucket = gate

			done := make(chan error, 1)
			go func() { done <- tc.write(svc, *group.GroupName, *other.Arn) }()
			<-gate.reached

			_, err = peerIAMService(t, svc).DetachGroupPolicy(testAccountID, &iam.DetachGroupPolicyInput{GroupName: group.GroupName, PolicyArn: policy.Arn})
			require.NoError(t, err)

			close(gate.release)
			require.NoError(t, <-done)

			got, err := svc.getGroup(t.Context(), testAccountID, *group.GroupName)
			require.NoError(t, err)
			assert.NotContains(t, got.AttachedPolicies, *policy.Arn, "the write must not reattach the detached policy")
		})
	}
}

// gatedJS hands out the account IAM bucket wrapped in gate, so a test can park
// an OIDC provider writer between its read and its write.
type gatedJS struct {
	jetstream.JetStream

	bucket string
	gate   *gatedKV
}

func (g *gatedJS) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	if bucket == g.bucket {
		return g.gate, nil
	}
	return g.JetStream.KeyValue(ctx, bucket)
}

// An OIDC provider deleted between a tag call's read and its write must stay
// deleted, and the tag call must report NoSuchEntity.
func TestOIDCProviderTagWriters_DoNotRecreateDeletedProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		write func(svc *IAMServiceImpl, arn string) error
	}{
		{"Tag", func(svc *IAMServiceImpl, arn string) error {
			_, err := svc.TagOpenIDConnectProvider(testAccountID, &iam.TagOpenIDConnectProviderInput{
				OpenIDConnectProviderArn: aws.String(arn),
				Tags:                     []*iam.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
			})
			return err
		}},
		{"Untag", func(svc *IAMServiceImpl, arn string) error {
			_, err := svc.UntagOpenIDConnectProvider(testAccountID, &iam.UntagOpenIDConnectProviderInput{
				OpenIDConnectProviderArn: aws.String(arn),
				TagKeys:                  aws.StringSlice([]string{"k"}),
			})
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			out, err := svc.CreateOpenIDConnectProvider(testAccountID, &iam.CreateOpenIDConnectProviderInput{Url: aws.String(testIssuer)})
			require.NoError(t, err)
			arn := *out.OpenIDConnectProviderArn

			kv, err := svc.js.KeyValue(t.Context(), IAMAccountBucketName(testAccountID))
			require.NoError(t, err)
			gate := &gatedKV{
				KeyValue: kv,
				key:      OIDCProviderKey(testIssuer),
				reached:  make(chan struct{}),
				release:  make(chan struct{}),
			}
			gate.armed.Store(true)
			svc.js = &gatedJS{JetStream: svc.js, bucket: IAMAccountBucketName(testAccountID), gate: gate}

			done := make(chan error, 1)
			go func() { done <- tc.write(svc, arn) }()
			<-gate.reached

			_, err = svc.DeleteOpenIDConnectProvider(testAccountID, &iam.DeleteOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(arn)})
			require.NoError(t, err)

			close(gate.release)
			err = <-done
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())

			_, err = kv.Get(t.Context(), OIDCProviderKey(testIssuer))
			assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "the deleted provider must stay deleted")
		})
	}
}

// contendedKV lands a competing write ahead of each of the first budget record
// updates it forwards, the order a caller looping TagUser wins every race in.
type contendedKV struct {
	jetstream.KeyValue

	svc     *IAMServiceImpl
	user    string
	budget  atomic.Int32
	armed   atomic.Bool
	pending sync.WaitGroup
	errs    chan error
}

func (c *contendedKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if c.armed.CompareAndSwap(true, false) && c.budget.Add(-1) >= 0 {
		done := make(chan struct{})
		c.pending.Go(func() {
			defer close(done)
			_, err := c.svc.TagUser(testAccountID, &iam.TagUserInput{
				UserName: aws.String(c.user),
				Tags:     []*iam.Tag{{Key: aws.String("k"), Value: aws.String(key)}},
			})
			c.errs <- err
		})
		// Either the competing write lands first or it queues behind this one.
		for c.svc.recordLocks.holders("user/"+key) < 2 {
			select {
			case <-done:
			case <-time.After(time.Millisecond):
				continue
			}
			break
		}
		c.armed.Store(true)
	}
	return c.KeyValue.Update(ctx, key, value, last)
}

func (l *recordLocks) holders(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rl := l.locks[key]; rl != nil {
		return rl.refs
	}
	return 0
}

// A self-scoped write that wins every revision race on this node must queue
// behind an administrator's DetachUserPolicy rather than exhaust its retries.
func TestDetachUserPolicy_IsNotStarvedByALoopingSelfWrite(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	user := createTestUser(t, svc, "self-tagger")
	policy := createTestPolicy(t, svc, "RevokedPolicy")
	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{UserName: user.UserName, PolicyArn: policy.Arn})
	require.NoError(t, err)

	kv := &contendedKV{KeyValue: svc.usersBucket, svc: svc, user: *user.UserName, errs: make(chan error, roleCASMaxRetries)}
	kv.budget.Store(roleCASMaxRetries)
	kv.armed.Store(true)
	svc.usersBucket = kv

	_, err = svc.DetachUserPolicy(testAccountID, &iam.DetachUserPolicyInput{UserName: user.UserName, PolicyArn: policy.Arn})
	require.NoError(t, err, "a contending self-write starved the detach")
	kv.pending.Wait()
	close(kv.errs)
	for werr := range kv.errs {
		require.NoError(t, werr)
	}

	got, err := svc.getUser(t.Context(), testAccountID, *user.UserName)
	require.NoError(t, err)
	assert.Empty(t, got.AttachedPolicies)
}
