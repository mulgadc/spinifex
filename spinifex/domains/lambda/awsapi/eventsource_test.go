package awsapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/eventsource"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSources struct {
	mu         sync.Mutex
	visibility int64
	err        error
	asRole     []string
}

func (f *fakeSources) QueueVisibilityTimeout(_ context.Context, rec function.Record, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asRole = append(f.asRole, rec.Role)
	return f.visibility, f.err
}

const mappingsPath = "/2015-03-31/event-source-mappings/"

var queueARN = "arn:aws:sqs:" + gwtest.Region + ":" + gwtest.AccountID + ":orders"

func newMappingEnv(t *testing.T, statements ...iamdomain.Statement) (*env, *eventsource.Store, *fakeSources) {
	t.Helper()
	e := newEnv(t, statements...)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) { b["Timeout"] = 30 }))
	require.NoError(t, err)
	_, _, js := testutil.StartTestJetStream(t)
	store, err := eventsource.Open(t.Context(), js)
	require.NoError(t, err)
	src := &fakeSources{visibility: 30}
	e.deps.Mappings = func(context.Context) (awsapi.MappingStore, error) { return store, nil }
	e.deps.Sources = src
	return e, store, src
}

func mappingBody(mutate func(map[string]any)) map[string]any {
	b := map[string]any{"FunctionName": "hello", "EventSourceArn": queueARN}
	if mutate != nil {
		mutate(b)
	}
	return b
}

func decodeMapping(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestEventSourceMapping_CreateGetListDelete(t *testing.T) {
	e, store, src := newMappingEnv(t)
	rec, err := e.do(http.MethodPost, mappingsPath, mappingBody(func(b map[string]any) { b["BatchSize"] = 4 }))
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, rec.Code)
	created := decodeMapping(t, rec.Body.Bytes())
	assert.Equal(t, "Creating", created["State"])
	assert.EqualValues(t, 4, created["BatchSize"])
	assert.Equal(t, queueARN, created["EventSourceArn"])
	assert.Equal(t, []string{roleARN}, src.asRole, "the queue is read as the function's execution role")
	uuid, _ := created["UUID"].(string)

	stored, _, err := store.Get(t.Context(), uuid)
	require.NoError(t, err)
	assert.Equal(t, eventsource.StateEnabled, stored.State)

	rec, err = e.do(http.MethodGet, mappingsPath+uuid, nil)
	require.NoError(t, err)
	assert.Equal(t, "Enabled", decodeMapping(t, rec.Body.Bytes())["State"])

	_, err = e.do(http.MethodPost, mappingsPath, mappingBody(nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)

	rec, err = e.do(http.MethodGet, mappingsPath+"?FunctionName=hello", nil)
	require.NoError(t, err)
	assert.Len(t, decodeMapping(t, rec.Body.Bytes())["EventSourceMappings"], 1)
	rec, err = e.do(http.MethodGet, mappingsPath+"?FunctionName=other", nil)
	require.NoError(t, err)
	assert.Empty(t, decodeMapping(t, rec.Body.Bytes())["EventSourceMappings"])

	for range 2 {
		rec, err = e.do(http.MethodDelete, mappingsPath+uuid, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusAccepted, rec.Code)
		assert.Equal(t, "Deleting", decodeMapping(t, rec.Body.Bytes())["State"])
	}
	stored, _, err = store.Get(t.Context(), uuid)
	require.NoError(t, err, "delete leaves the record for the poller to remove")
	assert.Equal(t, eventsource.StateDeleting, stored.State)
}

func TestEventSourceMapping_ConcurrentDuplicateCreatesYieldOneMapping(t *testing.T) {
	first, store, src := newMappingEnv(t)
	const n = 8
	gateways := []*env{first}
	for len(gateways) < n {
		e := newEnv(t)
		_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) { b["Timeout"] = 30 }))
		require.NoError(t, err)
		e.deps.Mappings = first.deps.Mappings
		e.deps.Sources = src
		gateways = append(gateways, e)
	}
	codes := make(chan error, n)
	var wg sync.WaitGroup
	for _, e := range gateways {
		wg.Go(func() {
			_, err := e.do(http.MethodPost, mappingsPath, mappingBody(nil))
			codes <- err
		})
	}
	wg.Wait()
	close(codes)
	created := 0
	for err := range codes {
		if err == nil {
			created++
			continue
		}
		testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	}
	assert.Equal(t, 1, created)
	entries, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestEventSourceMapping_RefusesWhatThisBuildDoesNotImplement(t *testing.T) {
	e, store, _ := newMappingEnv(t)
	cases := map[string]func(map[string]any){
		"FIFO queue":      func(b map[string]any) { b["EventSourceArn"] = queueARN + ".fifo" },
		"another account": func(b map[string]any) { b["EventSourceArn"] = "arn:aws:sqs:" + gwtest.Region + ":210987654321:orders" },
		"another Region":  func(b map[string]any) { b["EventSourceArn"] = "arn:aws:sqs:us-east-1:" + gwtest.AccountID + ":orders" },
		"not SQS": func(b map[string]any) {
			b["EventSourceArn"] = "arn:aws:kinesis:" + gwtest.Region + ":" + gwtest.AccountID + ":stream/s"
		},
		"filter criteria":   func(b map[string]any) { b["FilterCriteria"] = map[string]any{"Filters": []any{}} },
		"disabled":          func(b map[string]any) { b["Enabled"] = false },
		"batching window":   func(b map[string]any) { b["MaximumBatchingWindowInSeconds"] = 5 },
		"batch over 10":     func(b map[string]any) { b["BatchSize"] = 11 },
		"batch zero":        func(b map[string]any) { b["BatchSize"] = 0 },
		"qualified name":    func(b map[string]any) { b["FunctionName"] = "hello:1" },
		"missing source":    func(b map[string]any) { delete(b, "EventSourceArn") },
		"missing function":  func(b map[string]any) { delete(b, "FunctionName") },
		"starting position": func(b map[string]any) { b["StartingPosition"] = "LATEST" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.do(http.MethodPost, mappingsPath, mappingBody(mutate))
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
		})
	}
	entries, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestEventSourceMapping_VisibilityTimeoutMustCoverTheFunctionTimeout(t *testing.T) {
	e, store, src := newMappingEnv(t)
	src.visibility = 29
	_, err := e.do(http.MethodPost, mappingsPath, mappingBody(nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)

	src.visibility, src.err = 0, awserrors.Errorf(awserrors.ErrorAccessDeniedException, "denied")
	_, err = e.do(http.MethodPost, mappingsPath, mappingBody(nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorAccessDeniedException)

	entries, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, entries)

	_, err = e.do(http.MethodPost, mappingsPath, mappingBody(func(b map[string]any) { b["FunctionName"] = "absent" }))
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestEventSourceMapping_AnotherAccountsMappingIsNotFound(t *testing.T) {
	e, store, _ := newMappingEnv(t)
	m := eventsource.Mapping{
		FormatVersion: eventsource.FormatVersion, UUID: "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", AccountID: "210987654321",
		Region: gwtest.Region, FunctionName: "hello", EventSourceARN: "arn:aws:sqs:" + gwtest.Region + ":210987654321:orders",
		BatchSize: 1, State: eventsource.StateEnabled,
	}
	require.NoError(t, store.Create(t.Context(), m))
	_, err := e.do(http.MethodGet, mappingsPath+m.UUID, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	_, err = e.do(http.MethodDelete, mappingsPath+m.UUID, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	rec, err := e.do(http.MethodGet, mappingsPath, nil)
	require.NoError(t, err)
	assert.Empty(t, decodeMapping(t, rec.Body.Bytes())["EventSourceMappings"])
	_, err = e.do(http.MethodGet, mappingsPath+"not-a-uuid", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestEventSourceMapping_GetAndDeleteAuthorizeAgainstTheMappingARN(t *testing.T) {
	const uuid = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	mapping := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":event-source-mapping:" + uuid
	e, store, _ := newMappingEnv(t, gwtest.Statement("Allow", "*", "*"), gwtest.Statement("Deny", "lambda:*", mapping))
	require.NoError(t, store.Create(t.Context(), eventsource.Mapping{
		FormatVersion: eventsource.FormatVersion, UUID: uuid, AccountID: gwtest.AccountID, Region: gwtest.Region,
		FunctionName: "hello", EventSourceARN: queueARN, BatchSize: 1, State: eventsource.StateEnabled,
	}))
	_, err := e.do(http.MethodGet, mappingsPath+uuid, nil)
	gwtest.AssertDenied(t, err)
	_, err = e.do(http.MethodDelete, mappingsPath+uuid, nil)
	gwtest.AssertDenied(t, err)
	m, _, err := store.Get(t.Context(), uuid)
	require.NoError(t, err)
	assert.Equal(t, eventsource.StateEnabled, m.State)
	_, err = e.do(http.MethodGet, mappingsPath, nil)
	require.NoError(t, err, "List authorizes against *, not the denied mapping")
}
