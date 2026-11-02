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
	failCodeStored bool
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
	return r.Record, r.Revision, nil
}

func (c *localClient) CodeStored(ctx context.Context, account, region, name string, rev uint64, sha string, size int64) (uint64, error) {
	c.mu.Lock()
	fail := c.failCodeStored
	c.mu.Unlock()
	if fail {
		return 0, errors.New("nats: timeout")
	}
	r, err := c.svc.CodeStored(ctx, &function.CodeStoredRequest{Region: region, FunctionName: name, Revision: rev, SHA256: sha, Size: size}, account)
	if err != nil {
		return 0, err
	}
	return r.Revision, nil
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
}

var _ awsapi.CodeStore = (*memStore)(nil)

func (m *memStore) Put(_ context.Context, ref codestore.Ref, code []byte) (string, error) {
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

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	got, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "no AWS code on %v", err)
	assert.Equal(t, code, got)
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
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Contains(t, err.Error(), "Function already exist: hello")

	assert.Equal(t, 1, e.store.count(), "the loser wrote no code")
	after, _, err := e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.NoError(t, err)
	assert.Equal(t, first, after)
}

func TestCreateFunction_CodePutFailureFreesTheName(t *testing.T) {
	e := newEnv(t)
	e.store.putErr = errors.New("predastore down")
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	requireCode(t, err, awserrors.ErrorServiceException)
	assert.NotContains(t, err.Error(), "predastore down")
	assert.Equal(t, 1, e.meta.rollbacks)
	_, _, err = e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.ErrorIs(t, err, function.ErrNotFound)

	e.store.putErr = nil
	rec, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestCreateFunction_CodeStoredFailureRollsBackOnce(t *testing.T) {
	e := newEnv(t)
	e.meta.failCodeStored = true
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	requireCode(t, err, awserrors.ErrorServiceException)
	assert.Equal(t, 1, e.meta.rollbacks)
	assert.Equal(t, 1, e.store.count(), "the code object is left in place, never deleted here")
	_, _, err = e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.ErrorIs(t, err, function.ErrNotFound, "the still-storing claim was withdrawn")
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
			requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
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
		requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
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
			requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
			assert.Zero(t, e.store.count())
		})
	}
}

func TestCreateFunction_RoleErrorsDoNotLeakAcrossAccounts(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) { b["Role"] = foreignRoleARN }))
	requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
	assert.Contains(t, err.Error(), "cannot be assumed by Lambda")
}

func TestCreateFunction_RefusesBytesThatAreNotAZip(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Code"] = map[string]any{"ZipFile": []byte("PK\x03\x04 not really a zip")}
	}))
	requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
	assert.Contains(t, err.Error(), "Could not unzip uploaded file")
	_, _, err = e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.ErrorIs(t, err, function.ErrNotFound)
	assert.Empty(t, e.store.objects)
}

func TestCreateFunction_RoleLambdaCannotAssumeRecordsNothing(t *testing.T) {
	e := newEnv(t)
	e.deps.Trusts = func(string, string) (bool, error) { return false, nil }
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
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
			requireCode(t, err, awserrors.ErrorServiceException)
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
	requireCode(t, err, awserrors.ErrorResourceNotFoundException)
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
			requireCode(t, err, awserrors.ErrorResourceNotFoundException)
		})
	}

	_, err = e.do(http.MethodGet, "/2015-03-31/functions/"+esc(full+":$LATEST")+"?Qualifier=3", nil)
	requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/"+esc("arn:aws:s3:::bucket"), nil)
	requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
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
	requireCode(t, err, awserrors.ErrorServiceException)
	assert.NotContains(t, rec.Body.String(), "Location")
}

func TestGetFunction_NotFoundNamesTheARN(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodGet, "/2015-03-31/functions/ghost", nil)
	requireCode(t, err, awserrors.ErrorResourceNotFoundException)
	assert.Contains(t, err.Error(), "Function not found: arn:aws:lambda:"+gwtest.Region+":"+gwtest.AccountID+":function:ghost")
}

func TestGetFunction_FunctionsAreRegional(t *testing.T) {
	e := newEnv(t)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", nil))
	require.NoError(t, err)
	e.h.Region = "us-west-2"
	_, err = e.do(http.MethodGet, "/2015-03-31/functions/hello", nil)
	requireCode(t, err, awserrors.ErrorResourceNotFoundException)
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
	assert.Equal(t, []string{"CreateFunction", "GetFunction"}, reg.Inventory.Registered)
	assert.Equal(t, "CreateFunction", reg.ResolveAction(httptest.NewRequest(http.MethodPost, "/2015-03-31/functions", nil)))
	assert.Equal(t, "GetFunction", reg.ResolveAction(httptest.NewRequest(http.MethodGet, "/2015-03-31/functions/x", nil)))
	assert.Empty(t, reg.ResolveAction(httptest.NewRequest(http.MethodDelete, "/2015-03-31/functions/x", nil)))
}
