package awsapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const updatePath = "/2015-03-31/functions/hello/code"

var updateZip = buildZip("hello, version two")

func codeSHA(code []byte) string {
	sum := sha256.Sum256(code)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// activate moves every stored function to Active through the owner's own pass.
func (e *env) activate(t *testing.T) {
	t.Helper()
	e.meta.svc.SetReadiness(func(context.Context, function.Record) error { return nil })
	require.NoError(t, e.meta.svc.ActivatePending(t.Context()))
}

// record reads the stored function record and its revision.
func (e *env) record(t *testing.T, name string) (function.Record, uint64) {
	t.Helper()
	entry, err := e.kv(t).Get(t.Context(), function.KVFunctionKey(gwtest.Region, name))
	require.NoError(t, err)
	var rec function.Record
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	return rec, entry.Revision()
}

// newUpdateEnv creates an Active "hello" with an environment and non-default configuration.
func newUpdateEnv(t *testing.T, statements ...iamdomain.Statement) *env {
	t.Helper()
	e := newEnv(t, statements...)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Environment"] = map[string]any{"Variables": map[string]string{"MODE": "prod"}}
		b["Description"] = "demo"
		b["Timeout"] = 30
		b["MemorySize"] = 256
	}))
	require.NoError(t, err)
	e.activate(t)
	return e
}

func updateBody(mutate func(map[string]any)) map[string]any {
	b := map[string]any{"ZipFile": updateZip}
	if mutate != nil {
		mutate(b)
	}
	return b
}

func decodeConfig(t *testing.T, raw []byte) lambda.FunctionConfiguration {
	t.Helper()
	var cfg lambda.FunctionConfiguration
	require.NoError(t, json.Unmarshal(raw, &cfg))
	return cfg
}

// requireUnchanged asserts the record still names the original copy at its original revision.
func requireUnchanged(t *testing.T, e *env, before function.Record, beforeRev uint64) {
	t.Helper()
	after, rev := e.record(t, "hello")
	assert.Equal(t, beforeRev, rev, "the function record was written")
	assert.Equal(t, before.Code, after.Code)
}

func TestUpdateFunctionCode_SwapsTheCodeAndKeepsEverythingElse(t *testing.T) {
	e := newUpdateEnv(t)
	before, beforeRev := e.record(t, "hello")

	rec, err := e.do(http.MethodPut, updatePath, updateBody(func(b map[string]any) {
		b["Architectures"] = []string{"x86_64"}
		b["RevisionId"] = strconv.FormatUint(beforeRev, 10)
	}))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	cfg := decodeConfig(t, rec.Body.Bytes())
	assert.Equal(t, codeSHA(updateZip), *cfg.CodeSha256)
	assert.Equal(t, int64(len(updateZip)), *cfg.CodeSize)
	assert.Equal(t, "Active", *cfg.State)
	assert.Equal(t, "Successful", *cfg.LastUpdateStatus)
	assert.Equal(t, "prod", *cfg.Environment.Variables["MODE"])
	assert.Equal(t, "demo", *cfg.Description)
	assert.Equal(t, int64(30), *cfg.Timeout)
	assert.Equal(t, int64(256), *cfg.MemorySize)
	assert.Equal(t, roleARN, *cfg.Role)

	after, afterRev := e.record(t, "hello")
	assert.Equal(t, strconv.FormatUint(afterRev, 10), *cfg.RevisionId)
	assert.Greater(t, afterRev, beforeRev)
	assert.Equal(t, before.FunctionID, after.FunctionID)
	assert.Equal(t, before.EnvCipher, after.EnvCipher)
	assert.Equal(t, before.CreatedAt, after.CreatedAt)
	assert.NotEqual(t, before.Code.RevisionID, after.Code.RevisionID)
	assert.Equal(t, function.CodeStored, after.Code.State)
	assert.Equal(t, function.StateActive, after.State)

	// Both copies stay, each under its own revision and tracked.
	require.Equal(t, 2, e.store.count())
	assert.Equal(t, testZip, e.store.objects[codestore.Ref{AccountID: gwtest.AccountID, FunctionID: before.FunctionID, RevisionID: before.Code.RevisionID}])
	assert.Equal(t, updateZip, e.store.objects[codestore.Ref{AccountID: gwtest.AccountID, FunctionID: after.FunctionID, RevisionID: after.Code.RevisionID}])
	e.requireTracked(t)
	ups := e.uploadsByRevision(t)
	assert.Equal(t, function.UploadAttached, ups[before.Code.RevisionID].State)
	assert.Equal(t, function.UploadAttached, ups[after.Code.RevisionID].State)
	assert.Equal(t, afterRev, ups[after.Code.RevisionID].AttachedRevision)

	got, err := e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
	require.NoError(t, err)
	var out lambda.GetFunctionOutput
	require.NoError(t, json.Unmarshal(got.Body.Bytes(), &out))
	assert.Equal(t, codeSHA(updateZip), *out.Configuration.CodeSha256)
}

func TestUpdateFunctionCode_RefusesWhatThisBuildDoesNotImplement(t *testing.T) {
	cases := map[string]func(map[string]any){
		"S3 bucket":           func(b map[string]any) { delete(b, "ZipFile"); b["S3Bucket"] = "code" },
		"S3 key":              func(b map[string]any) { b["S3Key"] = "fn.zip" },
		"S3 object version":   func(b map[string]any) { b["S3ObjectVersion"] = "1" },
		"image":               func(b map[string]any) { delete(b, "ZipFile"); b["ImageUri"] = "repo/fn:latest" },
		"publish":             func(b map[string]any) { b["Publish"] = true },
		"arm64":               func(b map[string]any) { b["Architectures"] = []string{"arm64"} },
		"two architectures":   func(b map[string]any) { b["Architectures"] = []string{"x86_64", "arm64"} },
		"empty architectures": func(b map[string]any) { b["Architectures"] = []string{} },
		"unknown arch":        func(b map[string]any) { b["Architectures"] = []string{"sparc"} },
		"no source":           func(b map[string]any) { delete(b, "ZipFile") },
		"not a zip":           func(b map[string]any) { b["ZipFile"] = []byte("plain bytes") },
		"unmodelled member":   func(b map[string]any) { b["SourceKMSKeyArn"] = "arn:aws:kms:x" },
	}
	e := newUpdateEnv(t)
	before, beforeRev := e.record(t, "hello")
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.do(http.MethodPut, updatePath, updateBody(mutate))
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
		})
	}
	t.Run("publish names versions", func(t *testing.T) {
		_, err := e.do(http.MethodPut, updatePath, updateBody(func(b map[string]any) { b["Publish"] = true }))
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
		assert.Contains(t, errMessage(err), "Publish")
		assert.Contains(t, errMessage(err), "versions are not yet supported")
	})
	t.Run("qualified name", func(t *testing.T) {
		_, err := e.do(http.MethodPut, "/2015-03-31/functions/hello:1/code", updateBody(nil))
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	})
	requireUnchanged(t, e, before, beforeRev)
	assert.Equal(t, 1, e.store.count())
	assert.Len(t, e.uploadsByRevision(t), 1)
}

func TestUpdateFunctionCode_MissingOrInactiveFunction(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPut, updatePath, updateBody(nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)

	_, err = e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	before, beforeRev := e.record(t, "hello")
	_, err = e.do(http.MethodPut, updatePath, updateBody(nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Contains(t, errMessage(err), "Pending")
	requireUnchanged(t, e, before, beforeRev)
	assert.Equal(t, 1, e.store.count())
}

func TestUpdateFunctionCode_Authorization(t *testing.T) {
	arn := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
	t.Run("denied on the function ARN", func(t *testing.T) {
		e := newUpdateEnv(t, gwtest.Statement("Allow", "*", "*"), gwtest.Statement("Deny", "lambda:UpdateFunctionCode", arn))
		before, beforeRev := e.record(t, "hello")
		_, err := e.do(http.MethodPut, updatePath, updateBody(nil))
		gwtest.AssertDenied(t, err)
		requireUnchanged(t, e, before, beforeRev)
		assert.Equal(t, 1, e.store.count())
	})
	t.Run("another function's grant does not reach it", func(t *testing.T) {
		e := newUpdateEnv(t, gwtest.Statement("Allow", "lambda:CreateFunction", "*"), gwtest.Statement("Allow", "iam:PassRole", "*"),
			gwtest.Statement("Allow", "lambda:UpdateFunctionCode", arn+"-other"))
		_, err := e.do(http.MethodPut, updatePath, updateBody(nil))
		gwtest.AssertDenied(t, err)
	})
	t.Run("allowed on the function ARN", func(t *testing.T) {
		e := newUpdateEnv(t, gwtest.Statement("Allow", "lambda:CreateFunction", "*"), gwtest.Statement("Allow", "iam:PassRole", "*"),
			gwtest.Statement("Allow", "lambda:UpdateFunctionCode", arn))
		_, err := e.do(http.MethodPut, updatePath, updateBody(nil))
		require.NoError(t, err)
	})
}

func TestUpdateFunctionCode_DryRunWritesNothing(t *testing.T) {
	e := newUpdateEnv(t)
	before, beforeRev := e.record(t, "hello")
	rec, err := e.do(http.MethodPut, updatePath, updateBody(func(b map[string]any) { b["DryRun"] = true }))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	cfg := decodeConfig(t, rec.Body.Bytes())
	assert.Equal(t, codeSHA(testZip), *cfg.CodeSha256, "a dry run reports the code that still runs")
	assert.Equal(t, strconv.FormatUint(beforeRev, 10), *cfg.RevisionId)
	requireUnchanged(t, e, before, beforeRev)
	assert.Equal(t, 1, e.store.count())
	assert.Len(t, e.uploadsByRevision(t), 1)

	_, err = e.do(http.MethodPut, updatePath, updateBody(func(b map[string]any) { b["DryRun"] = true; b["Publish"] = true }))
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
}

func TestUpdateFunctionCode_StaleRevisionIDIsAPreconditionFailure(t *testing.T) {
	e := newUpdateEnv(t)
	before, beforeRev := e.record(t, "hello")
	_, err := e.do(http.MethodPut, updatePath, updateBody(func(b map[string]any) {
		b["RevisionId"] = strconv.FormatUint(beforeRev-1, 10)
	}))
	testutil.RequireAWSCode(t, err, awserrors.ErrorPreconditionFailedException)
	requireUnchanged(t, e, before, beforeRev)
	assert.Equal(t, 1, e.store.count())
	assert.Len(t, e.uploadsByRevision(t), 1)
}

func TestUpdateFunctionCode_FailedCopyLeavesTheFunctionOnItsOldCode(t *testing.T) {
	e := newUpdateEnv(t)
	before, beforeRev := e.record(t, "hello")
	e.store.putErr = errors.New("predastore: unavailable")
	_, err := e.do(http.MethodPut, updatePath, updateBody(nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	requireUnchanged(t, e, before, beforeRev)

	ups := e.uploadsByRevision(t)
	require.Len(t, ups, 2, "the attempted upload stays recorded")
	for rev, up := range ups {
		if rev == before.Code.RevisionID {
			assert.Equal(t, function.UploadAttached, up.State)
			continue
		}
		assert.Equal(t, function.UploadOwed, up.State, "an upload whose copy may exist stays owed")
	}
}

func TestUpdateFunctionCode_OwnerWithoutTheSubjectIsRetryableAndChangesNothing(t *testing.T) {
	e := newUpdateEnv(t)
	before, beforeRev := e.record(t, "hello")
	e.meta.beginErr = fmt.Errorf("NATS request to %s: %w", function.SubjectCodeUpdateBegin, nats.ErrNoResponders)
	_, err := e.do(http.MethodPut, updatePath, updateBody(nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.False(t, awserrors.IsTerminal(err), "the caller is told to retry once the owner is upgraded")
	requireUnchanged(t, e, before, beforeRev)
	assert.Equal(t, 1, e.store.count())
	assert.Len(t, e.uploadsByRevision(t), 1)
}

func TestUpdateFunctionCode_CompetingUpdatesCommitOnce(t *testing.T) {
	e := newUpdateEnv(t)
	other := buildZip("hello, competing version")
	// The second update starts and completes while the first is writing its copy.
	e.store.beforePut = func() {
		_, err := e.do(http.MethodPut, updatePath, updateBody(func(b map[string]any) { b["ZipFile"] = other }))
		assert.NoError(t, err)
	}
	_, err := e.do(http.MethodPut, updatePath, updateBody(nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)

	after, _ := e.record(t, "hello")
	cfg, err := e.do(http.MethodGet, "/2015-03-31/functions/hello/configuration", nil)
	require.NoError(t, err)
	assert.Equal(t, codeSHA(other), *decodeConfig(t, cfg.Body.Bytes()).CodeSha256, "the winner's code stays")
	assert.Equal(t, 3, e.store.count(), "the original, the winner's and the loser's copies")
	e.requireTracked(t)
	owed := 0
	for rev, up := range e.uploadsByRevision(t) {
		if up.State == function.UploadOwed {
			owed++
			assert.NotEqual(t, after.Code.RevisionID, rev)
		}
	}
	assert.Equal(t, 1, owed, "only the losing upload is owed")
}

func TestUpdateFunctionCode_LostCommitReplyIsAnErrorButTheUpdateStands(t *testing.T) {
	e := newUpdateEnv(t)
	before, _ := e.record(t, "hello")
	e.meta.loseUpdatedReply = true
	_, err := e.do(http.MethodPut, updatePath, updateBody(nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)

	after, _ := e.record(t, "hello")
	assert.NotEqual(t, before.Code.RevisionID, after.Code.RevisionID, "the committed update is not rolled back")
	cfg, err := e.do(http.MethodGet, "/2015-03-31/functions/hello/configuration", nil)
	require.NoError(t, err)
	assert.Equal(t, codeSHA(updateZip), *decodeConfig(t, cfg.Body.Bytes()).CodeSha256)
	require.Equal(t, 2, e.store.count(), "neither copy is deleted")
	ups := e.uploadsByRevision(t)
	require.Len(t, ups, 2)
	assert.Equal(t, function.UploadAttached, ups[before.Code.RevisionID].State)
	assert.Equal(t, function.UploadAttached, ups[after.Code.RevisionID].State)
}

func TestUpdateFunctionCode_AnInvocationInFlightKeepsItsOldCopy(t *testing.T) {
	e := newUpdateEnv(t)
	ex := &fakeExec{result: okResult, started: make(chan struct{}), release: make(chan struct{})}
	e.deps.Exec = ex
	before, _ := e.record(t, "hello")
	done := make(chan error, 1)
	go func() {
		_, err := e.invoke(t.Context(), invokePath, nil, nil)
		done <- err
	}()
	<-ex.started

	_, err := e.do(http.MethodPut, updatePath, updateBody(nil))
	require.NoError(t, err)
	close(ex.release)
	require.NoError(t, <-done)

	ex.mu.Lock()
	inflight := ex.calls[0].Function.Code
	ex.started = nil
	ex.mu.Unlock()
	assert.Equal(t, before.Code, inflight, "the running attempt stays bound to the copy it read")
	assert.Equal(t, testZip, e.store.objects[codestore.Ref{AccountID: gwtest.AccountID, FunctionID: before.FunctionID, RevisionID: inflight.RevisionID}],
		"the old copy is still there for it")

	_, err = e.invoke(t.Context(), invokePath, nil, nil)
	require.NoError(t, err)
	after, _ := e.record(t, "hello")
	ex.mu.Lock()
	defer ex.mu.Unlock()
	assert.Equal(t, after.Code, ex.calls[1].Function.Code, "a later attempt reads the new copy")
	assert.NotEqual(t, before.Code.RevisionID, ex.calls[1].Function.Code.RevisionID)
}
