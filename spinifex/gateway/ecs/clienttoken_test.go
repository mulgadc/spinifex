//test:in-package — runTaskWithClientToken and runTaskParamHash are unexported,
//and the rest of this package's tests are in-package for the same reason.

package gateway_ecs

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/idempotency"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tokenTestAccount = "123456789012"

func newRunTaskStore(t *testing.T) *runTaskStore {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	store, err := idempotency.OpenStore[ecs.RunTaskOutput](
		t.Context(), testutil.NewJetStream(t, nc), "ecs-tokens", time.Minute)
	require.NoError(t, err)
	return store
}

func taskOutput(ids ...string) ecs.RunTaskOutput {
	out := ecs.RunTaskOutput{}
	for _, id := range ids {
		out.Tasks = append(out.Tasks, &ecs.Task{TaskArn: aws.String(id)})
	}
	return out
}

// The bead's criterion for RunTask: one token, two calls, one placement. A
// second placement would double the reserved capacity and leak an ENI per task.
func TestRunTaskWithClientToken_PlacesOnce(t *testing.T) {
	t.Parallel()
	store := newRunTaskStore(t)
	const tok, hash = "tok-1", "h"
	launches := 0
	launch := func() (ecs.RunTaskOutput, error) {
		launches++
		return taskOutput("task-a", "task-b"), nil
	}

	first, err := runTaskWithClientToken(t.Context(), store, tokenTestAccount, tok, hash, launch)
	require.NoError(t, err)
	second, err := runTaskWithClientToken(t.Context(), store, tokenTestAccount, tok, hash, launch)
	require.NoError(t, err)

	assert.Equal(t, 1, launches, "the duplicate must replay, not place again")
	require.Len(t, second.Tasks, 2)
	assert.Equal(t, first.Tasks[0].TaskArn, second.Tasks[0].TaskArn)
	assert.Equal(t, first.Tasks[1].TaskArn, second.Tasks[1].TaskArn)
}

// A placement where some tasks started and others failed is still a placement:
// re-running would add tasks on top of the ones already running.
func TestRunTaskWithClientToken_PartialSuccessReplays(t *testing.T) {
	t.Parallel()
	store := newRunTaskStore(t)
	const tok, hash = "tok-partial", "h"
	partial := taskOutput("task-a")
	partial.Failures = []*ecs.Failure{{Reason: aws.String("RESOURCE:placement")}}

	launches := 0
	launch := func() (ecs.RunTaskOutput, error) {
		launches++
		return partial, nil
	}

	_, err := runTaskWithClientToken(t.Context(), store, tokenTestAccount, tok, hash, launch)
	require.NoError(t, err)
	replay, err := runTaskWithClientToken(t.Context(), store, tokenTestAccount, tok, hash, launch)
	require.NoError(t, err)

	assert.Equal(t, 1, launches, "a partial success must not be retried under the same token")
	require.Len(t, replay.Tasks, 1)
	require.Len(t, replay.Failures, 1, "the failures replay alongside the tasks")
	assert.Equal(t, "RESOURCE:placement", aws.StringValue(replay.Failures[0].Reason))
}

// A launch that errored placed nothing, so the token has to free up for a real
// retry rather than replay a result that never existed.
func TestRunTaskWithClientToken_FailedLaunchIsRetryable(t *testing.T) {
	t.Parallel()
	store := newRunTaskStore(t)
	const tok, hash = "tok-2", "h"
	boom := errors.New("placement failed")

	_, err := runTaskWithClientToken(t.Context(), store, tokenTestAccount, tok, hash,
		func() (ecs.RunTaskOutput, error) { return ecs.RunTaskOutput{}, boom })
	require.ErrorIs(t, err, boom, "the launch's own error reaches the caller unchanged")

	launches := 0
	out, err := runTaskWithClientToken(t.Context(), store, tokenTestAccount, tok, hash,
		func() (ecs.RunTaskOutput, error) {
			launches++
			return taskOutput("task-a"), nil
		})
	require.NoError(t, err)
	assert.Equal(t, 1, launches)
	require.Len(t, out.Tasks, 1)
}

// Reusing a token with different parameters is the AWS mismatch case, and must
// not place anything.
func TestRunTaskWithClientToken_ParamMismatchIsRejected(t *testing.T) {
	t.Parallel()
	store := newRunTaskStore(t)
	const tok = "tok-3"

	_, err := runTaskWithClientToken(t.Context(), store, tokenTestAccount, tok, "hash-a",
		func() (ecs.RunTaskOutput, error) { return taskOutput("task-a"), nil })
	require.NoError(t, err)

	launched := false
	_, err = runTaskWithClientToken(t.Context(), store, tokenTestAccount, tok, "hash-b",
		func() (ecs.RunTaskOutput, error) {
			launched = true
			return ecs.RunTaskOutput{}, nil
		})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIdempotentParameterMismatch, err.Error())
	assert.False(t, launched, "a mismatched token must not place tasks")
}

// The hash must ignore the token: a retry carries the same token and params, and
// changing Count is a different request even under the same token.
func TestRunTaskParamHash_IgnoresTheToken(t *testing.T) {
	t.Parallel()
	first := &ecs.RunTaskInput{Count: aws.Int64(2), ClientToken: aws.String("a")}
	sameParams := &ecs.RunTaskInput{Count: aws.Int64(2), ClientToken: aws.String("b")}
	changed := &ecs.RunTaskInput{Count: aws.Int64(5), ClientToken: aws.String("a")}

	assert.Equal(t, runTaskParamHash(first), runTaskParamHash(sameParams),
		"the token must not feed the hash")
	assert.NotEqual(t, runTaskParamHash(first), runTaskParamHash(changed),
		"a changed count must change the hash")
}

// Hashing must not disturb the input, which is dispatched to the launch after.
func TestRunTaskParamHash_LeavesTheInputIntact(t *testing.T) {
	t.Parallel()
	input := &ecs.RunTaskInput{Count: aws.Int64(2), ClientToken: aws.String("tok")}

	runTaskParamHash(input)

	require.NotNil(t, input.ClientToken)
	assert.Equal(t, "tok", *input.ClientToken)
}

// --- runTaskIdempotent -------------------------------------------------------

func tokenTestInput(token string) *ecs.RunTaskInput {
	input := &ecs.RunTaskInput{TaskDefinition: aws.String("web:1")}
	if token != "" {
		input.ClientToken = aws.String(token)
	}
	return input
}

func placingService(arn string) *fakeECSService {
	out := taskOutput(arn)
	return &fakeECSService{describeOut: taskDefWithRole(), runOut: &out}
}

// A request with no token must not pay for the store, let alone fail when the
// store is unreachable: AWS treats an absent token as opting out.
func TestRunTaskIdempotent_NoTokenRunsUnwrapped(t *testing.T) {
	t.Parallel()
	svc := placingService("task-a")
	openStore := func() (*runTaskStore, error) {
		t.Error("the store must not be opened for an untokened request")
		return nil, nil
	}

	out, err := runTaskIdempotent(t.Context(), svc, tokenTestAccount, tokenTestInput(""),
		allowCheck(t, testTaskRoleARN), openStore)

	require.NoError(t, err)
	assert.True(t, svc.runCalled, "an untokened request still places")
	require.Len(t, out.Tasks, 1)
}

// With no store there is no way to tell a retry from a first attempt, so placing
// anyway would create exactly the duplicate the token was sent to prevent.
func TestRunTaskIdempotent_StoreFailureRefusesToPlace(t *testing.T) {
	t.Parallel()
	svc := placingService("task-a")
	openStore := func() (*runTaskStore, error) { return nil, errors.New("jetstream down") }

	_, err := runTaskIdempotent(t.Context(), svc, tokenTestAccount, tokenTestInput("tok-store"),
		allowCheck(t, testTaskRoleARN), openStore)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.False(t, svc.runCalled, "a request that cannot be deduplicated must not place")
}

// The whole path, from the token on the input through to a replayed output.
func TestRunTaskIdempotent_PlacesOnceAcrossRetries(t *testing.T) {
	t.Parallel()
	store := newRunTaskStore(t)
	openStore := func() (*runTaskStore, error) { return store, nil }

	first := placingService("task-a")
	firstOut, err := runTaskIdempotent(t.Context(), first, tokenTestAccount, tokenTestInput("tok-retry"),
		allowCheck(t, testTaskRoleARN), openStore)
	require.NoError(t, err)
	require.Len(t, firstOut.Tasks, 1)

	// A retry is a fresh service: the replay must come from the store, not from
	// any state the first call left on the fake.
	second := placingService("task-b")
	replay, err := runTaskIdempotent(t.Context(), second, tokenTestAccount, tokenTestInput("tok-retry"),
		allowCheck(t, testTaskRoleARN), openStore)
	require.NoError(t, err)

	assert.False(t, second.runCalled, "the retry must replay rather than place again")
	require.Len(t, replay.Tasks, 1)
	assert.Equal(t, "task-a", aws.StringValue(replay.Tasks[0].TaskArn))
}

// A launch error must reach the caller unwrapped, and must not be recorded as a
// placement that a retry would replay.
func TestRunTaskIdempotent_LaunchErrorPropagates(t *testing.T) {
	t.Parallel()
	store := newRunTaskStore(t)
	openStore := func() (*runTaskStore, error) { return store, nil }
	svc := &fakeECSService{describeOut: taskDefWithRole()}

	_, err := runTaskIdempotent(t.Context(), svc, tokenTestAccount, tokenTestInput("tok-denied"),
		denyCheck(t, testTaskRoleARN), openStore)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
	assert.False(t, svc.runCalled)
}

// The store binds once per process, so a second caller must get the first
// caller's store rather than rebind the bucket.
func TestGetRunTaskStore_BindsOnce(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	first, err := getRunTaskStore(t.Context(), nc)
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := getRunTaskStore(t.Context(), nc)
	require.NoError(t, err)
	assert.Same(t, first, second, "the bucket must be bound once, not per request")
}
