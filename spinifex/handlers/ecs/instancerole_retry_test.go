// Drives ensureECSRolePolicy directly and swaps the unexported retry sleep, so
// it needs the package's own scope.
//
//test:in-package
package handlers_ecs

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flakyAttachIAM answers NoSuchEntity from AttachRolePolicy a set number of
// times before succeeding, standing in for a replica that has not yet caught up
// with the policy written immediately before.
type flakyAttachIAM struct {
	stubIAM

	notFoundTimes int
	attaches      int
}

func (f *flakyAttachIAM) AttachRolePolicy(_ string, _ *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	f.attaches++
	if f.attaches <= f.notFoundTimes {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.AttachRolePolicyOutput{}, nil
}

// alwaysDeniedIAM answers a code the retry must not swallow.
type alwaysDeniedIAM struct {
	stubIAM

	attaches int
}

func (a *alwaysDeniedIAM) AttachRolePolicy(_ string, _ *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	a.attaches++
	return nil, errors.New(awserrors.ErrorAccessDenied)
}

func noAttachSleep(t *testing.T) {
	t.Helper()
	original := ecsAttachRetrySleep
	ecsAttachRetrySleep = func(time.Duration) {}
	t.Cleanup(func() { ecsAttachRetrySleep = original })
}

// The policy is created and then attached. A NoSuchEntity from that attach is a
// stale read of a write that landed, so provisioning must converge rather than
// fail the whole call.
func TestEnsureECSRolePolicyRetriesAStaleNoSuchEntity(t *testing.T) {
	noAttachSleep(t)
	fake := &flakyAttachIAM{stubIAM: newStubIAM(), notFoundTimes: 3}
	svc := &Service{deps: Deps{IAM: fake}}

	require.NoError(t, svc.ensureECSRolePolicy("000000000106"))
	assert.Equal(t, 4, fake.attaches, "three stale reads then the one that sees the policy")
}

// The retry is bounded: a policy that genuinely never appears still fails.
func TestEnsureECSRolePolicyGivesUpOnAPersistentNoSuchEntity(t *testing.T) {
	noAttachSleep(t)
	fake := &flakyAttachIAM{stubIAM: newStubIAM(), notFoundTimes: 1000}
	svc := &Service{deps: Deps{IAM: fake}}

	err := svc.ensureECSRolePolicy("000000000106")
	require.Error(t, err)
	assert.ErrorContains(t, err, awserrors.ErrorIAMNoSuchEntity)
	assert.Equal(t, ecsAttachRetries+1, fake.attaches, "bounded by ecsAttachRetries")
}

// Only the stale-read code is retried. Anything else is a real answer and must
// come back on the first attempt.
func TestEnsureECSRolePolicyDoesNotRetryOtherErrors(t *testing.T) {
	noAttachSleep(t)
	fake := &alwaysDeniedIAM{stubIAM: newStubIAM()}
	svc := &Service{deps: Deps{IAM: fake}}

	err := svc.ensureECSRolePolicy("000000000106")
	require.Error(t, err)
	assert.ErrorContains(t, err, awserrors.ErrorAccessDenied)
	assert.Equal(t, 1, fake.attaches, "a denial is not a visibility race")
}
