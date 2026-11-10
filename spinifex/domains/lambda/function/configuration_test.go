package function

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configReq(revision uint64, cfg Configuration) *ConfigurationUpdatedRequest {
	return &ConfigurationUpdatedRequest{Region: testRegion, FunctionName: "hello", FunctionID: "f1", Revision: revision, Configuration: cfg}
}

var newConfig = Configuration{Role: "arn:aws:iam::000000000001:role/other", Handler: "handler.two", Description: "after",
	Timeout: 8, MemorySize: 256, EnvCipher: "sealed-after"}

func TestConfigurationUpdate_ChangesOnlyTheSettings(t *testing.T) {
	svc, c := newTestService(t)
	before := mustServe(t, svc)
	c.t = c.t.Add(time.Minute)

	resp, err := svc.UpdateConfiguration(t.Context(), configReq(before.Revision, newConfig), testAccount)
	require.NoError(t, err)
	got := mustGet(t, svc, "hello")
	assert.Equal(t, resp.Revision, got.Revision)
	assert.Equal(t, got.Record, resp.Record)

	want := before.Record
	want.Role, want.Handler, want.Description = newConfig.Role, newConfig.Handler, newConfig.Description
	want.Timeout, want.MemorySize, want.EnvCipher = newConfig.Timeout, newConfig.MemorySize, newConfig.EnvCipher
	want.LastModified = c.t
	assert.Equal(t, want, got.Record, "identity, code, state and creation time are unchanged")

	cleared := newConfig
	cleared.EnvCipher, cleared.Description = "", ""
	_, err = svc.UpdateConfiguration(t.Context(), configReq(got.Revision, cleared), testAccount)
	require.NoError(t, err)
	again := mustGet(t, svc, "hello").Record
	assert.Empty(t, again.EnvCipher)
	assert.Empty(t, again.Description)
}

func TestConfigurationUpdate_StaleRevisionIsRefused(t *testing.T) {
	svc, _ := newTestService(t)
	before := mustServe(t, svc)
	_, err := svc.UpdateConfiguration(t.Context(), configReq(before.Revision, newConfig), testAccount)
	require.NoError(t, err)
	after := mustGet(t, svc, "hello")

	other := newConfig
	other.Handler = "handler.three"
	_, err = svc.UpdateConfiguration(t.Context(), configReq(before.Revision, other), testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Equal(t, after, mustGet(t, svc, "hello"))
}

// The loser passes readServing at the shared revision, then pauses before its
// conditional write while the winner commits.
func TestConfigurationUpdate_InterleavedUpdateLosesTheConditionalWrite(t *testing.T) {
	svc, c := newTestService(t)
	before := mustServe(t, svc)
	paused, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	svc.now = func() time.Time {
		if calls.Add(1) == 1 {
			close(paused)
			<-release
		}
		return c.now()
	}
	loser := newConfig
	loser.Handler = "handler.loser"
	loserErr := make(chan error, 1)
	go func() {
		_, err := svc.UpdateConfiguration(t.Context(), configReq(before.Revision, loser), testAccount)
		loserErr <- err
	}()
	<-paused
	won, err := svc.UpdateConfiguration(t.Context(), configReq(before.Revision, newConfig), testAccount)
	require.NoError(t, err)
	close(release)
	testutil.RequireAWSCode(t, <-loserErr, awserrors.ErrorResourceConflictException)

	got := mustGet(t, svc, "hello")
	assert.Equal(t, won.Revision, got.Revision)
	assert.Equal(t, newConfig.Handler, got.Record.Handler)
}

func TestConfigurationUpdate_RefusesAFunctionThatIsNotServing(t *testing.T) {
	svc, _ := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")
	_, err := svc.UpdateConfiguration(t.Context(), configReq(rev, newConfig), testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Equal(t, rev, mustGet(t, svc, "hello").Revision)

	ghost := configReq(1, newConfig)
	ghost.FunctionName = "ghost"
	_, err = svc.UpdateConfiguration(t.Context(), ghost, testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)

	replaced := configReq(rev, newConfig)
	replaced.FunctionID = "f-other"
	_, err = svc.UpdateConfiguration(t.Context(), replaced, testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
}

func TestConfigurationUpdate_RefusesMalformedRequests(t *testing.T) {
	svc, _ := newTestService(t)
	before := mustServe(t, svc)
	cases := map[string]func(*ConfigurationUpdatedRequest){
		"region":      func(r *ConfigurationUpdatedRequest) { r.Region = "Bad Region" },
		"name":        func(r *ConfigurationUpdatedRequest) { r.FunctionName = "a/b" },
		"function id": func(r *ConfigurationUpdatedRequest) { r.FunctionID = "" },
		"role":        func(r *ConfigurationUpdatedRequest) { r.Configuration.Role = "" },
		"handler":     func(r *ConfigurationUpdatedRequest) { r.Configuration.Handler = "" },
		"timeout":     func(r *ConfigurationUpdatedRequest) { r.Configuration.Timeout = 0 },
		"memory":      func(r *ConfigurationUpdatedRequest) { r.Configuration.MemorySize = 0 },
	}
	for name, mutate := range cases {
		req := configReq(before.Revision, newConfig)
		mutate(req)
		t.Run(name, func(t *testing.T) {
			_, err := svc.UpdateConfiguration(t.Context(), req, testAccount)
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
		})
	}
	assert.Equal(t, before.Revision, mustGet(t, svc, "hello").Revision)
}
