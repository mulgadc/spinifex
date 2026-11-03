package awsapi_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/bluebottle/pkg/masterkey"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	roleARN        = "arn:aws:iam::" + gwtest.AccountID + ":role/lambda-exec"
	foreignRoleARN = "arn:aws:iam::999999999999:role/lambda-exec"
)

// localClient reaches a real MetaService without NATS.
type localClient struct {
	svc *function.MetaService

	mu             sync.Mutex
	forceActive    bool
	failCodeStored bool
	passOwnerClock bool
	rollbacks      int
}

var _ function.Client = (*localClient)(nil)

func (c *localClient) Create(ctx context.Context, account, region string, rec function.Record) (uint64, error) {
	r, err := c.svc.Create(ctx, &function.CreateRequest{Region: region, Record: rec}, account)
	if err != nil {
		return 0, err
	}
	return r.Revision, nil
}

func (c *localClient) Get(ctx context.Context, account, region, name string) (function.Record, uint64, error) {
	r, err := c.svc.Get(ctx, &function.GetRequest{Region: region, FunctionName: name}, account)
	if err != nil {
		return function.Record{}, 0, err
	}
	if !r.Found {
		return function.Record{}, 0, function.ErrNotFound
	}
	c.mu.Lock()
	if c.forceActive {
		r.Record.State = function.StateActive
	}
	c.mu.Unlock()
	return r.Record, r.Revision, nil
}

func (c *localClient) CodeStored(ctx context.Context, account, region, name string, rev uint64, sha string, size int64) (function.Record, uint64, error) {
	c.mu.Lock()
	fail, passClock := c.failCodeStored, c.passOwnerClock
	c.mu.Unlock()
	if fail {
		return function.Record{}, 0, errors.New("nats: timeout")
	}
	r, err := c.svc.CodeStored(ctx, &function.CodeStoredRequest{Region: region, FunctionName: name, Revision: rev, SHA256: sha, Size: size}, account)
	if err != nil {
		return function.Record{}, 0, err
	}
	// Let the wall clock pass the owner's millisecond, so a timestamp the
	// gateway reads for itself would differ from the stored one.
	for passClock && !time.Now().Truncate(time.Millisecond).After(r.Record.LastModified.Truncate(time.Millisecond)) {
		runtime.Gosched()
	}
	return r.Record, r.Revision, nil
}

func (c *localClient) RollbackCreate(ctx context.Context, account, region, name string, rev uint64) error {
	c.mu.Lock()
	c.rollbacks++
	c.mu.Unlock()
	_, err := c.svc.RollbackCreate(ctx, &function.RollbackCreateRequest{Region: region, FunctionName: name, Revision: rev}, account)
	return err
}

// memStore is an in-memory code store; objects is keyed by Ref.
type memStore struct {
	mu      sync.Mutex
	objects map[codestore.Ref][]byte
	putErr  error
	urlErr  error
	// beforePut runs once, unlocked, before the next write lands.
	beforePut func()
}

var _ awsapi.CodeStore = (*memStore)(nil)

func (m *memStore) Put(_ context.Context, ref codestore.Ref, code []byte) (string, error) {
	m.mu.Lock()
	hook := m.beforePut
	m.beforePut = nil
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return "", m.putErr
	}
	m.objects[ref] = append([]byte(nil), code...)
	sum := sha256.Sum256(code)
	return hex.EncodeToString(sum[:]), nil
}

func (m *memStore) PresignGet(_ context.Context, ref codestore.Ref, sha string, ttl time.Duration) (string, error) {
	if m.urlErr != nil {
		return "", m.urlErr
	}
	return "https://code.test/" + ref.FunctionID + "/" + sha + "?ttl=" + ttl.String(), nil
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

type roleSet map[string]bool

func (r roleSet) ResolveRole(accountID, arn string) (passrole.Role, error) {
	acct, name, err := auth.ParseRoleARN(arn)
	if err != nil {
		return passrole.Role{}, passrole.ErrMalformed
	}
	if acct != accountID {
		return passrole.Role{}, passrole.ErrForeignAccount
	}
	if !r[arn] {
		return passrole.Role{}, passrole.ErrNotFound
	}
	return passrole.Role{ARN: arn, Name: name}, nil
}

func newHarness(t *testing.T, statements ...iamdomain.Statement) (*gwtest.Harness, *localClient, *memStore, func(*testing.T) jetstream.KeyValue, *masterkey.Key) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	key, err := masterkey.New(make([]byte, 32))
	require.NoError(t, err)
	meta := &localClient{svc: function.NewMetaService(js)}
	store := &memStore{objects: map[codestore.Ref][]byte{}}
	kv := func(t *testing.T) jetstream.KeyValue {
		t.Helper()
		b, err := js.KeyValue(t.Context(), function.KVAccountBucket(gwtest.AccountID))
		require.NoError(t, err)
		return b
	}
	return gwtest.Scoped(statements...), meta, store, kv, key
}

// uploads returns every upload record in the account bucket, by function ID.
func (e *env) uploads(t *testing.T) map[string]function.Upload {
	t.Helper()
	kv := e.kv(t)
	keys, err := kv.ListKeys(t.Context())
	require.NoError(t, err)
	out := map[string]function.Upload{}
	for k := range keys.Keys() {
		if !strings.HasPrefix(k, "upload.") {
			continue
		}
		entry, err := kv.Get(t.Context(), k)
		require.NoError(t, err)
		var up function.Upload
		require.NoError(t, json.Unmarshal(entry.Value(), &up))
		require.Equal(t, function.KVUploadKey(up.Region, up.FunctionID, up.RevisionID), k)
		out[up.FunctionID] = up
	}
	return out
}

// requireTracked asserts every code copy in the store has an upload record.
func (e *env) requireTracked(t *testing.T) {
	t.Helper()
	ups := e.uploads(t)
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	for ref := range e.store.objects {
		up, ok := ups[ref.FunctionID]
		require.True(t, ok, "code copy %s has no upload record", ref.FunctionID)
		require.Equal(t, ref.RevisionID, up.RevisionID)
	}
}

type env struct {
	h     *gwtest.Harness
	meta  *localClient
	store *memStore
	kv    func(*testing.T) jetstream.KeyValue
	deps  awsapi.Deps
}

func newEnv(t *testing.T, statements ...iamdomain.Statement) *env {
	t.Helper()
	if len(statements) == 0 {
		statements = []iamdomain.Statement{gwtest.Statement("Allow", "*", "*")}
	}
	h, meta, store, kv, key := newHarness(t, statements...)
	e := &env{h: h, meta: meta, store: store, kv: kv}
	e.deps = awsapi.Deps{
		Meta: meta,
		Codes: func(context.Context) (awsapi.CodeStore, error) {
			return store, nil
		},
		Roles:  roleSet{roleARN: true},
		Trusts: func(string, string) (bool, error) { return true, nil },
		AEAD:   key.AEAD,
	}
	return e
}

func (e *env) do(method, path string, body any) (*httptest.ResponseRecorder, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	err := e.h.Dispatch(awsapi.NewRegistration(e.deps), awsapi.ServiceName, rec, req)
	return rec, err
}

var testZip = buildZip("hello")

func buildZip(content string) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create("bootstrap")
	if err == nil {
		_, err = f.Write([]byte(content))
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func mustZip(t *testing.T, content string) []byte {
	t.Helper()
	return buildZip(content)
}

func createBody(name string, mutate func(map[string]any)) map[string]any {
	b := map[string]any{
		"FunctionName": name,
		"Runtime":      "provided.al2023",
		"Role":         roleARN,
		"Handler":      "bootstrap",
		"Code":         map[string]any{"ZipFile": testZip},
	}
	if mutate != nil {
		mutate(b)
	}
	return b
}

func TestCreateFunction_Success(t *testing.T) {
	e := newEnv(t)
	code := testZip
	rec, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Environment"] = map[string]any{"Variables": map[string]string{"DB_PASSWORD": "hunter2"}}
		b["Description"] = "demo"
		b["Timeout"] = 30
		b["MemorySize"] = 256
	}))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)

	var cfg lambda.FunctionConfiguration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	sum := sha256.Sum256(code)
	assert.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), *cfg.CodeSha256, "AWS reports base64 of the raw digest")
	assert.Equal(t, int64(len(code)), *cfg.CodeSize)
	assert.Equal(t, "Pending", *cfg.State)
	assert.Equal(t, "Creating", *cfg.StateReasonCode)
	assert.Equal(t, "arn:aws:lambda:"+gwtest.Region+":"+gwtest.AccountID+":function:hello", *cfg.FunctionArn)
	assert.Equal(t, "$LATEST", *cfg.Version)
	assert.Equal(t, "Zip", *cfg.PackageType)
	assert.Equal(t, int64(30), *cfg.Timeout)
	assert.Equal(t, int64(256), *cfg.MemorySize)
	assert.Equal(t, "hunter2", *cfg.Environment.Variables["DB_PASSWORD"])
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}\+0000$`, *cfg.LastModified)

	require.Equal(t, 1, e.store.count())
	for _, stored := range e.store.objects {
		assert.Equal(t, code, stored, "the code copy is retrievable")
	}

	entry, err := e.kv(t).Get(t.Context(), function.KVFunctionKey(gwtest.Region, "hello"))
	require.NoError(t, err)
	assert.NotContains(t, string(entry.Value()), "hunter2", "the environment is never stored in plaintext")
	assert.Contains(t, string(entry.Value()), "envCipher")

	e.requireTracked(t)
	ups := e.uploads(t)
	require.Len(t, ups, 1)
	for _, up := range ups {
		assert.Equal(t, function.UploadAttached, up.State)
		assert.Equal(t, entry.Revision(), up.AttachedRevision)
		assert.Equal(t, "hello", up.FunctionName)
	}
}

func TestCreateFunction_RendersTheStoredTimestamps(t *testing.T) {
	e := newEnv(t)
	e.meta.passOwnerClock = true
	rec, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	var created lambda.FunctionConfiguration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	rec, err = e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
	require.NoError(t, err)
	var got lambda.GetFunctionOutput
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, *got.Configuration.LastModified, *created.LastModified)
	assert.Equal(t, *got.Configuration.RevisionId, *created.RevisionId)
}

func TestCreateFunction_DuplicateNameLeavesFirstUntouched(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	first, _, err := e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.NoError(t, err)

	_, err = e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Code"] = map[string]any{"ZipFile": mustZip(t, "different")}
	}))
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Contains(t, err.Error(), "Function already exist: hello")

	assert.Equal(t, 1, e.store.count(), "the loser wrote no code")
	assert.Len(t, e.uploads(t), 1, "the refused create leaves no upload record")
	e.requireTracked(t)
	after, _, err := e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.NoError(t, err)
	assert.Equal(t, first, after)
}

func TestCreateFunction_CodePutFailureFreesTheName(t *testing.T) {
	e := newEnv(t)
	e.store.putErr = errors.New("predastore down")
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.NotContains(t, err.Error(), "predastore down")
	assert.Equal(t, 1, e.meta.rollbacks)
	_, _, err = e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.ErrorIs(t, err, function.ErrNotFound)
	ups := e.uploads(t)
	require.Len(t, ups, 1)
	for _, up := range ups {
		assert.Equal(t, function.UploadOwed, up.State, "a failed write may still have landed")
	}

	e.store.putErr = nil
	rec, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestCreateFunction_CodeStoredFailureRollsBackOnce(t *testing.T) {
	e := newEnv(t)
	e.meta.failCodeStored = true
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.Equal(t, 1, e.meta.rollbacks)
	assert.Equal(t, 1, e.store.count(), "the code object is left in place, never deleted here")
	_, _, err = e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.ErrorIs(t, err, function.ErrNotFound, "the still-storing claim was withdrawn")
	e.requireTracked(t)
	for _, up := range e.uploads(t) {
		assert.Equal(t, function.UploadOwed, up.State, "the unnamed copy stays owed")
	}
}

// The first create's write stalls past its claim, a second create takes the
// name over, then the first write lands and its completion is refused.
func TestCreateFunction_LateWriterAfterTakeoverStaysTracked(t *testing.T) {
	e := newEnv(t)
	key := function.KVFunctionKey(gwtest.Region, "hello")
	e.store.beforePut = func() {
		entry, err := e.kv(t).Get(t.Context(), key)
		require.NoError(t, err)
		var rec function.Record
		require.NoError(t, json.Unmarshal(entry.Value(), &rec))
		rec.ClaimExpiresAt = time.Now().Add(-time.Second)
		data, err := json.Marshal(rec)
		require.NoError(t, err)
		_, err = e.kv(t).Update(t.Context(), key, data, entry.Revision())
		require.NoError(t, err)

		rec2, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
			b["Code"] = map[string]any{"ZipFile": mustZip(t, "second")}
		}))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, rec2.Code)
	}

	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	requireCode(t, err, awserrors.ErrorServiceException)
	assert.Equal(t, 2, e.store.count(), "both copies landed")
	e.requireTracked(t)

	winner, _, err := e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.NoError(t, err)
	ups := e.uploads(t)
	require.Len(t, ups, 2)
	for id, up := range ups {
		if id == winner.FunctionID {
			assert.Equal(t, function.UploadAttached, up.State)
			continue
		}
		assert.Equal(t, function.UploadOwed, up.State, "the late writer's copy is never inferred complete")
	}
}

func TestCreateFunction_RefusesUnsupportedInput(t *testing.T) {
	cases := map[string]func(map[string]any){
		"Code.S3Bucket":        func(b map[string]any) { b["Code"] = map[string]any{"S3Bucket": "b", "S3Key": "k"} },
		"Code.S3Key":           func(b map[string]any) { b["Code"] = map[string]any{"ZipFile": []byte("x"), "S3Key": "k"} },
		"Code.S3ObjectVersion": func(b map[string]any) { b["Code"] = map[string]any{"ZipFile": []byte("x"), "S3ObjectVersion": "v"} },
		"Code.ImageUri":        func(b map[string]any) { b["Code"] = map[string]any{"ImageUri": "x"} },
		"Code.SourceKMSKeyArn": func(b map[string]any) { b["Code"] = map[string]any{"ZipFile": []byte("x"), "SourceKMSKeyArn": "k"} },
		"Layers":               func(b map[string]any) { b["Layers"] = []string{"arn:aws:lambda:r:1:layer:l:1"} },
		"VpcConfig":            func(b map[string]any) { b["VpcConfig"] = map[string]any{"SubnetIds": []string{"subnet-1"}} },
		"KMSKeyArn":            func(b map[string]any) { b["KMSKeyArn"] = "arn:aws:kms:r:1:key/k" },
		"DeadLetterConfig":     func(b map[string]any) { b["DeadLetterConfig"] = map[string]any{"TargetArn": "arn:aws:sqs:r:1:q"} },
		"TracingConfig":        func(b map[string]any) { b["TracingConfig"] = map[string]any{"Mode": "Active"} },
		"FileSystemConfigs": func(b map[string]any) {
			b["FileSystemConfigs"] = []map[string]any{{"Arn": "a", "LocalMountPath": "/mnt/x"}}
		},
		"ImageConfig":          func(b map[string]any) { b["ImageConfig"] = map[string]any{"Command": []string{"x"}} },
		"Architectures":        func(b map[string]any) { b["Architectures"] = []string{"arm64"} },
		"Publish":              func(b map[string]any) { b["Publish"] = true },
		"SnapStart":            func(b map[string]any) { b["SnapStart"] = map[string]any{"ApplyOn": "PublishedVersions"} },
		"EphemeralStorage":     func(b map[string]any) { b["EphemeralStorage"] = map[string]any{"Size": 1024} },
		"CodeSigningConfigArn": func(b map[string]any) { b["CodeSigningConfigArn"] = "arn:aws:lambda:r:1:code-signing-config:c" },
		"LoggingConfig":        func(b map[string]any) { b["LoggingConfig"] = map[string]any{"LogFormat": "JSON"} },
		"Tags":                 func(b map[string]any) { b["Tags"] = map[string]string{"a": "b"} },
		"TenancyConfig":        func(b map[string]any) { b["TenancyConfig"] = map[string]any{"TenantIsolationMode": "PER_TENANT"} },
	}
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			e := newEnv(t)
			_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", mutate))
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
			assert.Zero(t, e.store.count())
			_, _, getErr := e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
			require.ErrorIs(t, getErr, function.ErrNotFound, "a refused request claims nothing")
		})
	}
}

func TestCreateFunction_AcceptsDefaultsEquivalentToAbsent(t *testing.T) {
	e := newEnv(t)
	rec, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Architectures"] = []string{"x86_64"}
		b["Publish"] = false
		b["EphemeralStorage"] = map[string]any{"Size": 512}
		b["TracingConfig"] = map[string]any{"Mode": "PassThrough"}
		b["PackageType"] = "Zip"
		b["Layers"] = []string{}
		b["Tags"] = map[string]string{}
		b["LoggingConfig"] = map[string]any{"LogFormat": "Text", "LogGroup": "/aws/lambda/hello"}
	}))
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestCreateFunction_RefusesNonDefaultLoggingConfig(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"LogGroup": "/custom/group"},
		{"LogFormat": "Text", "LogGroup": "/aws/lambda/other"},
		{"LogFormat": "Text", "ApplicationLogLevel": "DEBUG"},
		{"SystemLogLevel": "WARN"},
	} {
		e := newEnv(t)
		_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) { b["LoggingConfig"] = cfg }))
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
		_, _, getErr := e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
		require.ErrorIs(t, getErr, function.ErrNotFound, "%v claims nothing", cfg)
	}
}

func TestFunctionResponses_CarryTheDefaultLoggingConfig(t *testing.T) {
	e := newEnv(t)
	rec, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	var created lambda.FunctionConfiguration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	want := &lambda.LoggingConfig{LogFormat: aws.String("Text"), LogGroup: aws.String("/aws/lambda/hello")}
	assert.Equal(t, want, created.LoggingConfig)

	rec, err = e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
	require.NoError(t, err)
	var got lambda.GetFunctionOutput
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, want, got.Configuration.LoggingConfig)
}

func TestCreateFunction_ParameterValidation(t *testing.T) {
	cases := map[string]func(map[string]any){
		"bad name":        func(b map[string]any) { b["FunctionName"] = "has space" },
		"arn name":        func(b map[string]any) { b["FunctionName"] = "arn:aws:lambda:r:1:function:x" },
		"long name":       func(b map[string]any) { b["FunctionName"] = strings.Repeat("a", 65) },
		"runtime":         func(b map[string]any) { b["Runtime"] = "python3.12" },
		"no runtime":      func(b map[string]any) { delete(b, "Runtime") },
		"package type":    func(b map[string]any) { b["PackageType"] = "Image" },
		"no code":         func(b map[string]any) { delete(b, "Code") },
		"empty zip":       func(b map[string]any) { b["Code"] = map[string]any{"ZipFile": []byte{}} },
		"no handler":      func(b map[string]any) { delete(b, "Handler") },
		"no role":         func(b map[string]any) { delete(b, "Role") },
		"timeout low":     func(b map[string]any) { b["Timeout"] = 0 },
		"timeout high":    func(b map[string]any) { b["Timeout"] = 901 },
		"memory low":      func(b map[string]any) { b["MemorySize"] = 64 },
		"memory high":     func(b map[string]any) { b["MemorySize"] = 10241 },
		"cross account":   func(b map[string]any) { b["Role"] = foreignRoleARN },
		"missing role":    func(b map[string]any) { b["Role"] = "arn:aws:iam::" + gwtest.AccountID + ":role/ghost" },
		"not a role arn":  func(b map[string]any) { b["Role"] = "arn:aws:iam::" + gwtest.AccountID + ":user/alice" },
		"malformed body":  func(b map[string]any) { b["Timeout"] = "soon" },
		"empty env name":  func(b map[string]any) { b["Environment"] = map[string]any{"Variables": map[string]string{"": "x"}} },
		"description len": func(b map[string]any) { b["Description"] = strings.Repeat("d", 257) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", mutate))
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
			assert.Zero(t, e.store.count())
		})
	}
}

func TestCreateFunction_RoleErrorsDoNotLeakAcrossAccounts(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) { b["Role"] = foreignRoleARN }))
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	assert.Contains(t, err.Error(), "cannot be assumed by Lambda")
}

func TestCreateFunction_RefusesBytesThatAreNotAZip(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Code"] = map[string]any{"ZipFile": []byte("PK\x03\x04 not really a zip")}
	}))
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	assert.Contains(t, err.Error(), "Could not unzip uploaded file")
	_, _, err = e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.ErrorIs(t, err, function.ErrNotFound)
	assert.Empty(t, e.store.objects)
}

func TestCreateFunction_RoleLambdaCannotAssumeRecordsNothing(t *testing.T) {
	e := newEnv(t)
	e.deps.Trusts = func(string, string) (bool, error) { return false, nil }
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	assert.Contains(t, err.Error(), "cannot be assumed by Lambda")
	_, _, err = e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.ErrorIs(t, err, function.ErrNotFound)
	assert.Empty(t, e.store.objects)
}

func TestCreateFunction_MissingRoleCheckFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*awsapi.Deps){
		"no resolver":      func(d *awsapi.Deps) { d.Roles = nil },
		"no trust checker": func(d *awsapi.Deps) { d.Trusts = nil },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			mutate(&e.deps)
			_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
			testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
			_, _, err = e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
			require.ErrorIs(t, err, function.ErrNotFound)
		})
	}
}

func TestGetFunction_DecryptsEnvAndLocatesStoredCode(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Environment"] = map[string]any{"Variables": map[string]string{"MODE": "prod"}}
	}))
	require.NoError(t, err)

	rec, err := e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	var out lambda.GetFunctionOutput
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "prod", *out.Configuration.Environment.Variables["MODE"])
	assert.Equal(t, "S3", *out.Code.RepositoryType)
	require.NotNil(t, out.Code.Location)
	assert.Contains(t, *out.Code.Location, "https://code.test/")
	assert.Contains(t, *out.Code.Location, "ttl=10m0s")

	rec, err = e.do(http.MethodGet, "/2015-03-31/functions/hello?Qualifier=%24LATEST", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/hello?Qualifier=3", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestGetFunction_AcceptsSameAccountARNForms(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	full := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
	esc := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, ":", "%3A"), "$", "%24") }

	for _, name := range []string{full, full + ":$LATEST", gwtest.AccountID + ":function:hello", "hello:$LATEST"} {
		t.Run(name, func(t *testing.T) {
			rec, err := e.do(http.MethodGet, "/2015-03-31/functions/"+esc(name), nil)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rec.Code)
			var out lambda.GetFunctionOutput
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			assert.Equal(t, full, *out.Configuration.FunctionArn)
		})
	}

	notFound := map[string]string{
		"other account":   "arn:aws:lambda:" + gwtest.Region + ":999999999999:function:hello",
		"other region":    "arn:aws:lambda:us-west-2:" + gwtest.AccountID + ":function:hello",
		"partial foreign": "999999999999:function:hello",
		"version":         full + ":3",
	}
	for label, name := range notFound {
		t.Run(label, func(t *testing.T) {
			_, err := e.do(http.MethodGet, "/2015-03-31/functions/"+esc(name), nil)
			testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
		})
	}

	_, err = e.do(http.MethodGet, "/2015-03-31/functions/"+esc(full+":$LATEST")+"?Qualifier=3", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/"+esc("arn:aws:s3:::bucket"), nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
}

func TestGetFunction_NoLocationUntilCodeIsStored(t *testing.T) {
	e := newEnv(t)
	_, err := e.meta.Create(t.Context(), gwtest.AccountID, gwtest.Region, function.Record{
		FunctionID: "f1", FunctionName: "pending", Role: roleARN, Runtime: "provided.al2023", Handler: "h",
		Timeout: 3, MemorySize: 128, Code: function.Code{RevisionID: "r1"},
	})
	require.NoError(t, err)

	rec, err := e.do(http.MethodGet, "/2015-03-31/functions/pending", nil)
	require.NoError(t, err)
	var out lambda.GetFunctionOutput
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Nil(t, out.Code.Location)
	assert.Nil(t, out.Configuration.CodeSha256)
	assert.Equal(t, "Pending", *out.Configuration.State)
}

func TestGetFunction_FailsWithoutAnOffHostCodeURL(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	e.store.urlErr = codestore.ErrNoPresignEndpoint

	rec, err := e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.NotContains(t, rec.Body.String(), "Location")
}

func TestGetFunction_NotFoundNamesTheARN(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodGet, "/2015-03-31/functions/ghost", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	assert.Contains(t, err.Error(), "Function not found: arn:aws:lambda:"+gwtest.Region+":"+gwtest.AccountID+":function:ghost")
}

func TestGetFunction_FunctionsAreRegional(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	e.h.Region = "us-west-2"
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestAuthorization(t *testing.T) {
	create := func(e *env) error {
		_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
		return err
	}
	get := func(e *env) error {
		_, err := e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
		return err
	}

	t.Run("create needs lambda:CreateFunction", func(t *testing.T) {
		e := newEnv(t, gwtest.Statement("Allow", "lambda:GetFunction", "*"), gwtest.Statement("Allow", "iam:PassRole", "*"))
		gwtest.AssertDenied(t, create(e))
		assert.Zero(t, e.store.count())
	})
	t.Run("create needs iam:PassRole", func(t *testing.T) {
		e := newEnv(t, gwtest.Statement("Allow", "lambda:*", "*"))
		gwtest.AssertDenied(t, create(e))
		assert.Zero(t, e.store.count())
	})
	t.Run("create is scoped to the function ARN", func(t *testing.T) {
		arn := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
		e := newEnv(t,
			gwtest.Statement("Allow", "lambda:*", "*"),
			gwtest.Statement("Allow", "iam:PassRole", "*"),
			gwtest.Statement("Deny", "lambda:CreateFunction", arn))
		gwtest.AssertDenied(t, create(e))
	})
	t.Run("create passes with both grants", func(t *testing.T) {
		e := newEnv(t, gwtest.Statement("Allow", "lambda:CreateFunction", "*"), gwtest.Statement("Allow", "iam:PassRole", "*"))
		require.NoError(t, create(e))
	})
	t.Run("get needs lambda:GetFunction", func(t *testing.T) {
		e := newEnv(t, gwtest.Statement("Allow", "lambda:CreateFunction", "*"))
		gwtest.AssertDenied(t, get(e))
	})
	t.Run("a denied caller does not learn a role is absent", func(t *testing.T) {
		e := newEnv(t, gwtest.Statement("Allow", "lambda:*", "*"))
		_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
			b["Role"] = "arn:aws:iam::" + gwtest.AccountID + ":role/ghost"
		}))
		gwtest.AssertDenied(t, err)
	})
}

func TestRegistration_ResolvesActionsAndInventory(t *testing.T) {
	reg := awsapi.NewRegistration(awsapi.Deps{})
	assert.Equal(t, []string{"CreateFunction", "GetFunction", "GetFunctionCodeSigningConfig", "GetFunctionConfiguration", "Invoke", "ListVersionsByFunction"}, reg.Inventory.Registered)
	assert.Equal(t, "ListVersionsByFunction", reg.ResolveAction(httptest.NewRequest(http.MethodGet, "/2015-03-31/functions/x/versions", nil)))
	assert.Equal(t, "GetFunctionCodeSigningConfig", reg.ResolveAction(httptest.NewRequest(http.MethodGet, "/2020-06-30/functions/x/code-signing-config", nil)))
	assert.Equal(t, "CreateFunction", reg.ResolveAction(httptest.NewRequest(http.MethodPost, "/2015-03-31/functions", nil)))
	assert.Equal(t, "GetFunction", reg.ResolveAction(httptest.NewRequest(http.MethodGet, "/2015-03-31/functions/x", nil)))
	assert.Equal(t, "GetFunctionConfiguration", reg.ResolveAction(httptest.NewRequest(http.MethodGet, "/2015-03-31/functions/x/configuration", nil)))
	assert.Empty(t, reg.ResolveAction(httptest.NewRequest(http.MethodDelete, "/2015-03-31/functions/x", nil)))
}

func TestGetFunctionConfiguration_ReturnsTheSameConfigurationAsGetFunction(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Environment"] = map[string]any{"Variables": map[string]string{"MODE": "prod"}}
	}))
	require.NoError(t, err)

	rec, err := e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
	require.NoError(t, err)
	var full lambda.GetFunctionOutput
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &full))

	rec, err = e.do(http.MethodGet, "/2015-03-31/functions/hello/configuration", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"Code"`, "the configuration is the top-level body, not GetFunction's wrapper")
	var cfg lambda.FunctionConfiguration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	assert.Equal(t, *full.Configuration, cfg)
	assert.Equal(t, "prod", *cfg.Environment.Variables["MODE"])

	arn := "arn%3Aaws%3Alambda%3A" + gwtest.Region + "%3A" + gwtest.AccountID + "%3Afunction%3Ahello"
	for _, path := range []string{
		"/2015-03-31/functions/hello/configuration?Qualifier=%24LATEST",
		"/2015-03-31/functions/" + arn + "/configuration",
		"/2015-03-31/functions/" + arn + "%3A%24LATEST/configuration",
	} {
		rec, err := e.do(http.MethodGet, path, nil)
		require.NoError(t, err, path)
		assert.Equal(t, http.StatusOK, rec.Code, path)
	}
}

func TestGetFunctionConfiguration_MissingAndUnsupportedMatchGetFunction(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)

	for label, tc := range map[string]struct{ suffix, code string }{
		"missing":         {"ghost", awserrors.ErrorResourceNotFoundException},
		"version":         {"hello?Qualifier=3", awserrors.ErrorResourceNotFoundException},
		"other account":   {"arn%3Aaws%3Alambda%3A" + gwtest.Region + "%3A999999999999%3Afunction%3Ahello", awserrors.ErrorResourceNotFoundException},
		"qualifier clash": {"hello%3A%24LATEST?Qualifier=3", awserrors.ErrorInvalidParameterValueException},
		"not lambda":      {"arn%3Aaws%3As3%3A%3A%3Abucket", awserrors.ErrorInvalidParameterValueException},
	} {
		t.Run(label, func(t *testing.T) {
			path, query, _ := strings.Cut(tc.suffix, "?")
			if query != "" {
				query = "?" + query
			}
			_, getErr := e.do(http.MethodGet, "/2015-03-31/functions/"+path+query, nil)
			testutil.RequireAWSCode(t, getErr, tc.code)
			_, err := e.do(http.MethodGet, "/2015-03-31/functions/"+path+"/configuration"+query, nil)
			testutil.RequireAWSCode(t, err, tc.code)
			assert.Equal(t, getErr.Error(), err.Error())
		})
	}

	e.h.Region = "us-west-2"
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/hello/configuration", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestGetFunctionConfiguration_DoesNotNeedTheCodeStore(t *testing.T) {
	for name, codes := range map[string]awsapi.CodeStoreFunc{
		"store outage": func(context.Context) (awsapi.CodeStore, error) { return nil, errors.New("code store down") },
		"no store":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
			require.NoError(t, err)
			e.deps.Codes = codes

			rec, err := e.do(http.MethodGet, "/2015-03-31/functions/hello/configuration", nil)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rec.Code)
			var cfg lambda.FunctionConfiguration
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
			assert.Equal(t, "hello", *cfg.FunctionName)

			_, err = e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
			testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
		})
	}
}

func TestGetFunctionConfiguration_Authorization(t *testing.T) {
	arn := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
	setup := func(t *testing.T, statements ...iamdomain.Statement) *env {
		e := newEnv(t, statements...)
		_, err := e.meta.Create(t.Context(), gwtest.AccountID, gwtest.Region, function.Record{
			FunctionID: "f1", FunctionName: "hello", Role: roleARN, Runtime: "provided.al2023", Handler: "h",
			Timeout: 3, MemorySize: 128, Code: function.Code{RevisionID: "r1"},
		})
		require.NoError(t, err)
		return e
	}
	get := func(e *env) error {
		_, err := e.do(http.MethodGet, "/2015-03-31/functions/hello/configuration", nil)
		return err
	}

	t.Run("lambda:GetFunction alone is not enough", func(t *testing.T) {
		gwtest.AssertDenied(t, get(setup(t, gwtest.Statement("Allow", "lambda:GetFunction", "*"))))
	})
	t.Run("a deny on the function ARN applies", func(t *testing.T) {
		gwtest.AssertDenied(t, get(setup(t,
			gwtest.Statement("Allow", "lambda:*", "*"),
			gwtest.Statement("Deny", "lambda:GetFunctionConfiguration", arn))))
	})
	t.Run("an allow on another function ARN does not apply", func(t *testing.T) {
		gwtest.AssertDenied(t, get(setup(t, gwtest.Statement("Allow", "lambda:GetFunctionConfiguration", arn+"-other"))))
	})
	t.Run("an allow on the function ARN passes", func(t *testing.T) {
		require.NoError(t, get(setup(t, gwtest.Statement("Allow", "lambda:GetFunctionConfiguration", arn))))
	})
}

func TestGetFunctionCodeSigningConfig_ExistingFunctionHasExplicitNulls(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)

	arn := "arn%3Aaws%3Alambda%3A" + gwtest.Region + "%3A" + gwtest.AccountID + "%3Afunction%3Ahello"
	for _, name := range []string{"hello", arn, gwtest.AccountID + "%3Afunction%3Ahello"} {
		rec, err := e.do(http.MethodGet, "/2020-06-30/functions/"+name+"/code-signing-config", nil)
		require.NoError(t, err, name)
		require.Equal(t, http.StatusOK, rec.Code, name)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.JSONEq(t, `{"CodeSigningConfigArn":null,"FunctionName":null}`, rec.Body.String(), name)
	}
}

func TestGetFunctionCodeSigningConfig_MissingFunctionIsNotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)

	_, err = e.do(http.MethodGet, "/2020-06-30/functions/ghost/code-signing-config", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	_, getErr := e.do(http.MethodGet, "/2015-03-31/functions/ghost", nil)
	assert.Equal(t, getErr.Error(), err.Error())

	e.h.Region = "us-west-2"
	_, err = e.do(http.MethodGet, "/2020-06-30/functions/hello/code-signing-config", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestGetFunctionCodeSigningConfig_NeedsNeitherCodeStoreNorDecryption(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Environment"] = map[string]any{"Variables": map[string]string{"MODE": "prod"}}
	}))
	require.NoError(t, err)
	e.deps.Codes = func(context.Context) (awsapi.CodeStore, error) { return nil, errors.New("code store down") }
	e.deps.AEAD = nil

	rec, err := e.do(http.MethodGet, "/2020-06-30/functions/hello/code-signing-config", nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"CodeSigningConfigArn":null,"FunctionName":null}`, rec.Body.String())
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/hello/configuration", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)

	e.deps.Codes = nil
	_, err = e.do(http.MethodGet, "/2020-06-30/functions/hello/code-signing-config", nil)
	require.NoError(t, err)
}

func TestGetFunctionCodeSigningConfig_Authorization(t *testing.T) {
	arn := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
	setup := func(t *testing.T, statements ...iamdomain.Statement) *env {
		e := newEnv(t, statements...)
		_, err := e.meta.Create(t.Context(), gwtest.AccountID, gwtest.Region, function.Record{
			FunctionID: "f1", FunctionName: "hello", Role: roleARN, Runtime: "provided.al2023", Handler: "h",
			Timeout: 3, MemorySize: 128, Code: function.Code{RevisionID: "r1"},
		})
		require.NoError(t, err)
		return e
	}
	get := func(e *env) error {
		_, err := e.do(http.MethodGet, "/2020-06-30/functions/hello/code-signing-config", nil)
		return err
	}

	t.Run("lambda:GetFunction alone is not enough", func(t *testing.T) {
		gwtest.AssertDenied(t, get(setup(t, gwtest.Statement("Allow", "lambda:GetFunction", "*"))))
	})
	t.Run("a deny on the function ARN applies", func(t *testing.T) {
		gwtest.AssertDenied(t, get(setup(t,
			gwtest.Statement("Allow", "lambda:*", "*"),
			gwtest.Statement("Deny", "lambda:GetFunctionCodeSigningConfig", arn))))
	})
	t.Run("an allow on another function ARN does not apply", func(t *testing.T) {
		gwtest.AssertDenied(t, get(setup(t, gwtest.Statement("Allow", "lambda:GetFunctionCodeSigningConfig", arn+"-other"))))
	})
	t.Run("an allow on the function ARN passes", func(t *testing.T) {
		require.NoError(t, get(setup(t, gwtest.Statement("Allow", "lambda:GetFunctionCodeSigningConfig", arn))))
	})
}

func TestListVersionsByFunction_ListsOnlyLatest(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Environment"] = map[string]any{"Variables": map[string]string{"MODE": "prod"}}
	}))
	require.NoError(t, err)
	rec, err := e.do(http.MethodGet, "/2015-03-31/functions/hello/configuration", nil)
	require.NoError(t, err)
	var cfg lambda.FunctionConfiguration
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))

	arn := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
	for _, path := range []string{
		"/2015-03-31/functions/hello/versions",
		"/2015-03-31/functions/hello/versions?MaxItems=10000",
		"/2015-03-31/functions/hello/versions?MaxItems=1",
		"/2015-03-31/functions/" + url.PathEscape(arn) + "/versions",
	} {
		rec, err := e.do(http.MethodGet, path, nil)
		require.NoError(t, err, path)
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.NotContains(t, rec.Body.String(), "NextMarker", path)
		var out lambda.ListVersionsByFunctionOutput
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), path)
		require.Len(t, out.Versions, 1, path)
		v := out.Versions[0]
		assert.Equal(t, arn+":$LATEST", *v.FunctionArn, path)
		assert.Equal(t, "$LATEST", *v.Version, path)
		assert.Equal(t, "prod", *v.Environment.Variables["MODE"], path)
		v.FunctionArn = cfg.FunctionArn
		assert.Equal(t, cfg, *v, "apart from the qualified ARN it is the configuration read")
	}
}

func TestListVersionsByFunction_RefusesBadMaxItemsAndAnyMarker(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)

	for _, q := range []string{"MaxItems=0", "MaxItems=10001", "MaxItems=-1", "MaxItems=ten", "MaxItems=", "Marker=not-a-marker", "Marker="} {
		_, err := e.do(http.MethodGet, "/2015-03-31/functions/hello/versions?"+q, nil)
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	}
}

func TestListVersionsByFunction_MissingFunctionIsNotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)

	_, err = e.do(http.MethodGet, "/2015-03-31/functions/ghost/versions", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	_, getErr := e.do(http.MethodGet, "/2015-03-31/functions/ghost", nil)
	assert.Equal(t, getErr.Error(), err.Error())

	e.h.Region = "us-west-2"
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/hello/versions", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestListVersionsByFunction_DoesNotNeedTheCodeStore(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	e.deps.Codes = func(context.Context) (awsapi.CodeStore, error) { return nil, errors.New("code store down") }
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/hello/versions", nil)
	require.NoError(t, err)
	e.deps.Codes = nil
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/hello/versions", nil)
	require.NoError(t, err)
}

func TestListVersionsByFunction_Authorization(t *testing.T) {
	arn := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
	setup := func(t *testing.T, statements ...iamdomain.Statement) *env {
		e := newEnv(t, statements...)
		_, err := e.meta.Create(t.Context(), gwtest.AccountID, gwtest.Region, function.Record{
			FunctionID: "f1", FunctionName: "hello", Role: roleARN, Runtime: "provided.al2023", Handler: "h",
			Timeout: 3, MemorySize: 128, Code: function.Code{RevisionID: "r1"},
		})
		require.NoError(t, err)
		return e
	}
	list := func(e *env) error {
		_, err := e.do(http.MethodGet, "/2015-03-31/functions/hello/versions", nil)
		return err
	}

	t.Run("lambda:GetFunction alone is not enough", func(t *testing.T) {
		gwtest.AssertDenied(t, list(setup(t, gwtest.Statement("Allow", "lambda:GetFunction", "*"))))
	})
	t.Run("a deny on the function ARN applies", func(t *testing.T) {
		gwtest.AssertDenied(t, list(setup(t,
			gwtest.Statement("Allow", "lambda:*", "*"),
			gwtest.Statement("Deny", "lambda:ListVersionsByFunction", arn))))
	})
	t.Run("an allow on another function ARN does not apply", func(t *testing.T) {
		gwtest.AssertDenied(t, list(setup(t, gwtest.Statement("Allow", "lambda:ListVersionsByFunction", arn+"-other"))))
	})
	t.Run("an allow on the function ARN passes", func(t *testing.T) {
		require.NoError(t, list(setup(t, gwtest.Statement("Allow", "lambda:ListVersionsByFunction", arn))))
	})
}
