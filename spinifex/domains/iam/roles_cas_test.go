package iam

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatedKV parks the first Get of key after it reads, until release is closed,
// so a test can slot another writer between a read and its write.
type gatedKV struct {
	jetstream.KeyValue

	key     string
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func (g *gatedKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	entry, err := g.KeyValue.Get(ctx, key)
	if key == g.key && g.armed.CompareAndSwap(true, false) {
		close(g.reached)
		<-g.release
	}
	return entry, err
}

// peerIAMService is another node's IAM service over the same JetStream. It
// shares the records but not this service's record locks, so a writer parked
// by gatedKV still races it the way a second gateway would.
func peerIAMService(t *testing.T, svc *IAMServiceImpl) *IAMServiceImpl {
	t.Helper()
	masterKey, err := GenerateMasterKey()
	require.NoError(t, err)
	peer, err := NewIAMServiceImpl(t.Context(), svc.natsConn, masterKey)
	require.NoError(t, err)
	return peer
}

// A role update that read the record before a DetachRolePolicy and writes after
// it must not put the detached policy back.
func TestRoleUpdates_DoNotUndoConcurrentDetach(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		update func(svc *IAMServiceImpl, roleName string) error
	}{
		{"UpdateRole", func(svc *IAMServiceImpl, roleName string) error {
			_, err := svc.UpdateRole(testAccountID, &iam.UpdateRoleInput{
				RoleName:    aws.String(roleName),
				Description: aws.String("updated"),
			})
			return err
		}},
		{"UpdateRoleDescription", func(svc *IAMServiceImpl, roleName string) error {
			out, err := svc.UpdateRoleDescription(testAccountID, &iam.UpdateRoleDescriptionInput{
				RoleName:    aws.String(roleName),
				Description: aws.String("updated"),
			})
			if err == nil && aws.StringValue(out.Role.Description) != "updated" {
				t.Errorf("returned description = %q", aws.StringValue(out.Role.Description))
			}
			return err
		}},
		{"UpdateAssumeRolePolicy", func(svc *IAMServiceImpl, roleName string) error {
			_, err := svc.UpdateAssumeRolePolicy(testAccountID, &iam.UpdateAssumeRolePolicyInput{
				RoleName:       aws.String(roleName),
				PolicyDocument: aws.String(validTrustPolicy()),
			})
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			role := createTestRole(t, svc, "detach-race")
			policy := createTestPolicy(t, svc, "DetachRacePolicy")
			_, err := svc.AttachRolePolicy(testAccountID, &iam.AttachRolePolicyInput{
				RoleName:  role.RoleName,
				PolicyArn: policy.Arn,
			})
			require.NoError(t, err)

			gate := &gatedKV{
				KeyValue: svc.rolesBucket,
				key:      testAccountID + "." + *role.RoleName,
				reached:  make(chan struct{}),
				release:  make(chan struct{}),
			}
			gate.armed.Store(true)
			svc.rolesBucket = gate

			done := make(chan error, 1)
			go func() { done <- tc.update(svc, *role.RoleName) }()
			<-gate.reached

			_, err = peerIAMService(t, svc).DetachRolePolicy(testAccountID, &iam.DetachRolePolicyInput{
				RoleName:  role.RoleName,
				PolicyArn: policy.Arn,
			})
			require.NoError(t, err)

			close(gate.release)
			require.NoError(t, <-done)

			got, err := svc.getRole(t.Context(), testAccountID, *role.RoleName)
			require.NoError(t, err)
			assert.Empty(t, got.AttachedPolicies, "the update must not reattach the detached policy")
			if tc.name != "UpdateAssumeRolePolicy" {
				assert.Equal(t, "updated", got.Description, "the update itself must still land")
			}
		})
	}
}
