package awsapi_test

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	configPath   = "/2015-03-31/functions/hello/configuration"
	otherRoleARN = "arn:aws:iam::" + gwtest.AccountID + ":role/lambda-other"
)

// newConfigEnv is newUpdateEnv with a second role the caller may pass.
func newConfigEnv(t *testing.T) *env {
	t.Helper()
	e := newUpdateEnv(t)
	e.deps.Roles = roleSet{roleARN: true, otherRoleARN: true}
	return e
}

// requireConfigUnchanged asserts the record and its revision are as they were.
func requireConfigUnchanged(t *testing.T, e *env, before function.Record, beforeRev uint64) {
	t.Helper()
	after, rev := e.record(t, "hello")
	assert.Equal(t, beforeRev, rev, "the function record was written")
	assert.Equal(t, before, after)
}

func TestUpdateFunctionConfiguration_ChangesEachSettingAndKeepsTheFunction(t *testing.T) {
	e := newConfigEnv(t)
	before, beforeRev := e.record(t, "hello")

	rec, err := e.do(http.MethodPut, configPath, map[string]any{
		"Role": otherRoleARN, "Handler": "handler.two", "Description": "after", "Timeout": 8, "MemorySize": 512,
		"Environment": map[string]any{"Variables": map[string]string{"GREETING": "after"}},
		"Runtime":     "provided.al2023", "RevisionId": strconv.FormatUint(beforeRev, 10),
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	cfg := decodeConfig(t, rec.Body.Bytes())
	assert.Equal(t, otherRoleARN, *cfg.Role)
	assert.Equal(t, "handler.two", *cfg.Handler)
	assert.Equal(t, "after", *cfg.Description)
	assert.Equal(t, int64(8), *cfg.Timeout)
	assert.Equal(t, int64(512), *cfg.MemorySize)
	require.NotNil(t, cfg.Environment)
	assert.Len(t, cfg.Environment.Variables, 1, "a supplied environment replaces the variables")
	assert.Equal(t, "after", *cfg.Environment.Variables["GREETING"])
	assert.Equal(t, "Successful", *cfg.LastUpdateStatus)
	assert.Equal(t, codeSHA(testZip), *cfg.CodeSha256)

	after, afterRev := e.record(t, "hello")
	assert.Equal(t, strconv.FormatUint(afterRev, 10), *cfg.RevisionId)
	assert.Greater(t, afterRev, beforeRev)
	assert.Equal(t, before.FunctionID, after.FunctionID)
	assert.Equal(t, before.Code, after.Code)
	assert.Equal(t, before.CreatedAt, after.CreatedAt)
	assert.Equal(t, function.StateActive, after.State)
	assert.NotContains(t, after.EnvCipher, "after", "the environment is stored sealed")

	got, err := e.do(http.MethodGet, configPath, nil)
	require.NoError(t, err)
	read := decodeConfig(t, got.Body.Bytes())
	assert.Equal(t, *cfg.RevisionId, *read.RevisionId)
	assert.Equal(t, "after", *read.Environment.Variables["GREETING"])
	assert.NotContains(t, read.Environment.Variables, "MODE")
}

func TestUpdateFunctionConfiguration_OmittedMembersKeepTheirValues(t *testing.T) {
	e := newConfigEnv(t)
	before, _ := e.record(t, "hello")

	rec, err := e.do(http.MethodPut, configPath, map[string]any{"Description": "only this"})
	require.NoError(t, err)
	cfg := decodeConfig(t, rec.Body.Bytes())
	assert.Equal(t, "only this", *cfg.Description)
	assert.Equal(t, "prod", *cfg.Environment.Variables["MODE"])

	after, _ := e.record(t, "hello")
	want := before
	want.Description, want.LastModified = "only this", after.LastModified
	assert.Equal(t, want, after, "the omitted environment keeps its sealed value")
}

func TestUpdateFunctionConfiguration_EmptyVariablesClearTheEnvironment(t *testing.T) {
	e := newConfigEnv(t)
	rec, err := e.do(http.MethodPut, configPath, map[string]any{"Environment": map[string]any{"Variables": map[string]string{}}})
	require.NoError(t, err)
	assert.Nil(t, decodeConfig(t, rec.Body.Bytes()).Environment)
	after, _ := e.record(t, "hello")
	assert.Empty(t, after.EnvCipher)
}

// The limit is on the variables as a JSON object: {"KK":"<escaped value>"}.
func TestUpdateFunctionConfiguration_EnvironmentSizeCountsTheSerializedObject(t *testing.T) {
	quotes := strings.Repeat(`"`, 2043)
	for name, tc := range map[string]struct {
		value string
		ok    bool
	}{
		"at the limit":    {value: quotes + "a", ok: true},
		"one byte over":   {value: quotes + "ab", ok: false},
		"raw bytes under": {value: quotes + "\t", ok: false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newConfigEnv(t)
			before, beforeRev := e.record(t, "hello")
			_, err := e.do(http.MethodPut, configPath, map[string]any{"Environment": map[string]any{"Variables": map[string]string{"KK": tc.value}}})
			if tc.ok {
				require.NoError(t, err)
				return
			}
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
			assert.Contains(t, errMessage(err), "4KB")
			assert.NotContains(t, errMessage(err), `""`, "the message carries no variable value")
			requireConfigUnchanged(t, e, before, beforeRev)
		})
	}
}

func TestUpdateFunctionConfiguration_RefusesWhatThisBuildDoesNotImplement(t *testing.T) {
	for field, value := range map[string]any{
		"Layers":            []string{"arn:aws:lambda:us-east-1:000000000000:layer:x:1"},
		"VpcConfig":         map[string]any{"SubnetIds": []string{"subnet-1"}},
		"KMSKeyArn":         "arn:aws:kms:us-east-1:000000000000:key/k",
		"DeadLetterConfig":  map[string]any{"TargetArn": "arn:aws:sqs:us-east-1:000000000000:dlq"},
		"TracingConfig":     map[string]any{"Mode": "Active"},
		"FileSystemConfigs": []map[string]any{{"Arn": "arn:x", "LocalMountPath": "/mnt/x"}},
		"ImageConfig":       map[string]any{"Command": []string{"x"}},
		"EphemeralStorage":  map[string]any{"Size": 1024},
		"SnapStart":         map[string]any{"ApplyOn": "PublishedVersions"},
		"LoggingConfig":     map[string]any{"LogFormat": "JSON"},
		"Runtime":           "python3.12",
		"Unmodelled":        true,
	} {
		t.Run(field, func(t *testing.T) {
			e := newConfigEnv(t)
			before, beforeRev := e.record(t, "hello")
			_, err := e.do(http.MethodPut, configPath, map[string]any{field: value})
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
			requireConfigUnchanged(t, e, before, beforeRev)
		})
	}
}

func TestUpdateFunctionConfiguration_ParameterValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body map[string]any
		code string
	}{
		"handler with a space":  {map[string]any{"Handler": "bad handler"}, awserrors.ErrorValidationException},
		"handler with a NUL":    {map[string]any{"Handler": "bad\x00handler"}, awserrors.ErrorValidationException},
		"empty handler":         {map[string]any{"Handler": ""}, awserrors.ErrorValidationException},
		"handler over 128":      {map[string]any{"Handler": strings.Repeat("h", 129)}, awserrors.ErrorValidationException},
		"timeout zero":          {map[string]any{"Timeout": 0}, awserrors.ErrorInvalidParameterValueException},
		"timeout over 900":      {map[string]any{"Timeout": 901}, awserrors.ErrorInvalidParameterValueException},
		"memory under 128":      {map[string]any{"MemorySize": 127}, awserrors.ErrorInvalidParameterValueException},
		"memory over 10240":     {map[string]any{"MemorySize": 10241}, awserrors.ErrorInvalidParameterValueException},
		"description over 256":  {map[string]any{"Description": strings.Repeat("d", 257)}, awserrors.ErrorInvalidParameterValueException},
		"empty role":            {map[string]any{"Role": ""}, awserrors.ErrorInvalidParameterValueException},
		"environment name":      {map[string]any{"Environment": map[string]any{"Variables": map[string]string{"1BAD": "x"}}}, awserrors.ErrorValidationException},
		"reserved environment":  {map[string]any{"Environment": map[string]any{"Variables": map[string]string{"AWS_REGION": "x"}}}, awserrors.ErrorInvalidParameterValueException},
		"qualified name":        {nil, awserrors.ErrorInvalidParameterValueException},
		"stale revision":        {map[string]any{"Description": "x", "RevisionId": "1"}, awserrors.ErrorPreconditionFailedException},
		"handler at 128 is new": {map[string]any{"Handler": strings.Repeat("h", 128)}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := newConfigEnv(t)
			before, beforeRev := e.record(t, "hello")
			path := configPath
			if tc.body == nil {
				path = "/2015-03-31/functions/hello:1/configuration"
				tc.body = map[string]any{"Description": "x"}
			}
			_, err := e.do(http.MethodPut, path, tc.body)
			if tc.code == "" {
				require.NoError(t, err)
				return
			}
			testutil.RequireAWSCode(t, err, tc.code)
			requireConfigUnchanged(t, e, before, beforeRev)
			assert.Zero(t, e.meta.configCalls, "a refused request never reaches the owner")
		})
	}
}

func TestUpdateFunctionConfiguration_MissingOrInactiveFunction(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPut, configPath, map[string]any{"Description": "x"})
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)

	_, err = e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	before, beforeRev := e.record(t, "hello")
	_, err = e.do(http.MethodPut, configPath, map[string]any{"Description": "x"})
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Contains(t, errMessage(err), "Pending")
	requireConfigUnchanged(t, e, before, beforeRev)
}

func TestUpdateFunctionConfiguration_Authorization(t *testing.T) {
	arn := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
	allowAll := gwtest.Statement("Allow", "*", "*")
	t.Run("denied on the function ARN", func(t *testing.T) {
		e := newConfigEnv(t)
		e.h = gwtest.Scoped(allowAll, gwtest.Statement("Deny", "lambda:UpdateFunctionConfiguration", arn))
		before, beforeRev := e.record(t, "hello")
		_, err := e.do(http.MethodPut, configPath, map[string]any{"Description": "x"})
		gwtest.AssertDenied(t, err)
		requireConfigUnchanged(t, e, before, beforeRev)
	})
	t.Run("another function's grant does not reach it", func(t *testing.T) {
		e := newConfigEnv(t)
		e.h = gwtest.Scoped(gwtest.Statement("Allow", "lambda:UpdateFunctionConfiguration", arn+"-other"))
		_, err := e.do(http.MethodPut, configPath, map[string]any{"Description": "x"})
		gwtest.AssertDenied(t, err)
	})
	t.Run("a new role needs PassRole on it", func(t *testing.T) {
		e := newConfigEnv(t)
		e.h = gwtest.Scoped(allowAll, gwtest.Statement("Deny", "iam:PassRole", otherRoleARN))
		before, beforeRev := e.record(t, "hello")
		_, err := e.do(http.MethodPut, configPath, map[string]any{"Role": otherRoleARN})
		gwtest.AssertDenied(t, err)
		requireConfigUnchanged(t, e, before, beforeRev)

		_, err = e.do(http.MethodPut, configPath, map[string]any{"Description": "no role change"})
		require.NoError(t, err, "an update that keeps the role passes no role")
	})
	t.Run("allowed on the function ARN", func(t *testing.T) {
		e := newConfigEnv(t)
		e.h = gwtest.Scoped(gwtest.Statement("Allow", "lambda:UpdateFunctionConfiguration", arn), gwtest.Statement("Allow", "iam:PassRole", otherRoleARN))
		_, err := e.do(http.MethodPut, configPath, map[string]any{"Role": otherRoleARN})
		require.NoError(t, err)
	})
}

func TestUpdateFunctionConfiguration_RefusesARoleLambdaCannotAssume(t *testing.T) {
	for name, tc := range map[string]struct {
		role   string
		trusts bool
	}{
		"untrusted":       {otherRoleARN, false},
		"missing":         {"arn:aws:iam::" + gwtest.AccountID + ":role/absent", true},
		"another account": {foreignRoleARN, true},
		"malformed":       {"not-a-role", true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newConfigEnv(t)
			e.deps.Trusts = func(string, string) (bool, error) { return tc.trusts, nil }
			before, beforeRev := e.record(t, "hello")
			_, err := e.do(http.MethodPut, configPath, map[string]any{"Role": tc.role})
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
			assert.Contains(t, errMessage(err), "cannot be assumed by Lambda")
			requireConfigUnchanged(t, e, before, beforeRev)
		})
	}
}

func TestUpdateFunctionConfiguration_CompetingUpdatesCommitOnce(t *testing.T) {
	e := newConfigEnv(t)
	// The second update starts and completes between the first one's read and its commit.
	e.meta.beforeCommit = func() {
		_, err := e.do(http.MethodPut, configPath, map[string]any{"Handler": "handler.winner"})
		assert.NoError(t, err)
	}
	_, err := e.do(http.MethodPut, configPath, map[string]any{"Handler": "handler.loser", "Timeout": 9})
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	after, _ := e.record(t, "hello")
	assert.Equal(t, "handler.winner", after.Handler)
	assert.Equal(t, int64(30), after.Timeout, "nothing of the losing update is written")
}

func TestUpdateFunctionConfiguration_LostReplyIsAnErrorButTheUpdateStands(t *testing.T) {
	e := newConfigEnv(t)
	e.meta.loseUpdatedReply = true
	_, err := e.do(http.MethodPut, configPath, map[string]any{"Handler": "handler.two"})
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.False(t, awserrors.IsTerminal(err))
	after, _ := e.record(t, "hello")
	assert.Equal(t, "handler.two", after.Handler, "the committed update is not rolled back")
}

func TestUpdateFunctionConfiguration_OwnerWithoutTheSubjectIsRetryableAndChangesNothing(t *testing.T) {
	e := newConfigEnv(t)
	before, beforeRev := e.record(t, "hello")
	e.meta.configErr = fmt.Errorf("NATS request to %s: %w", function.SubjectConfigurationUpdated, nats.ErrNoResponders)
	_, err := e.do(http.MethodPut, configPath, map[string]any{"Handler": "handler.two"})
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.False(t, awserrors.IsTerminal(err), "the caller is told to retry once the owner is upgraded")
	requireConfigUnchanged(t, e, before, beforeRev)
}

func TestUpdateFunctionConfiguration_UnreadableEnvironmentWritesNothing(t *testing.T) {
	e := newConfigEnv(t)
	before, beforeRev := e.record(t, "hello")
	e.deps.AEAD = nil
	_, err := e.do(http.MethodPut, configPath, map[string]any{"Description": "x"})
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	_, err = e.do(http.MethodPut, configPath, map[string]any{"Environment": map[string]any{"Variables": map[string]string{"AB": "c"}}})
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.Zero(t, e.meta.configCalls)
	requireConfigUnchanged(t, e, before, beforeRev)
}
