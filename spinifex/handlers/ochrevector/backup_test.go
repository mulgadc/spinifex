// Exercises unexported ochrevector backup/restore internals with no
// exported surface to drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const backupTestAccount = "123456789012"

// fakeDumper is the PgDumper test double: it records exactly what it was
// asked to dump/restore and lets tests script content/errors, so the
// object-key/gzip/streaming plumbing around it is provable without a live
// Postgres or the pg_dump/psql binaries.
type fakeDumper struct {
	dumpContent []byte
	dumpErr     error
	dumpDSN     string
	dumpSchema  string
	dumpCalls   int

	restoreErr     error
	restoreDSN     string
	restoreContent []byte
	restoreCalls   int
}

func (f *fakeDumper) Dump(_ context.Context, dsn, schema string, w io.Writer) error {
	f.dumpCalls++
	f.dumpDSN = dsn
	f.dumpSchema = schema
	if f.dumpErr != nil {
		return f.dumpErr
	}
	_, err := w.Write(f.dumpContent)
	return err
}

func (f *fakeDumper) Restore(_ context.Context, dsn string, r io.Reader) error {
	f.restoreCalls++
	f.restoreDSN = dsn
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.restoreContent = data
	return f.restoreErr
}

var _ PgDumper = (*fakeDumper)(nil)

// TestExecPgDumper_Dump_MissingBinaryPropagatesError points PATH at an empty
// directory so pg_dump can never be found, proving Dump builds the command
// and wraps exec's failure without needing a live Postgres or binary.
func TestExecPgDumper_Dump_MissingBinaryPropagatesError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var buf bytes.Buffer
	schema := schemaName(backupTestAccount)

	err := ExecPgDumper{}.Dump(context.Background(), "postgres://ignored", schema, &buf)
	require.Error(t, err)
	assert.ErrorContains(t, err, "pg_dump schema "+schema)
	assert.Zero(t, buf.Len())
}

// TestExecPgDumper_Restore_MissingBinaryPropagatesError is Dump's
// counterpart for Restore: psql is unreachable via PATH, so Restore must
// wrap exec's failure rather than hang on stdin.
func TestExecPgDumper_Restore_MissingBinaryPropagatesError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	err := ExecPgDumper{}.Restore(context.Background(), "postgres://ignored", strings.NewReader("select 1;"))
	require.Error(t, err)
	assert.ErrorContains(t, err, "psql restore")
}

// fakeAccountGranter is the AccountGranter test double: it records every
// EnsureAccount/RegrantAccount call so Restore's post-import repair sequence
// is provable without a live pgxBackend.
type fakeAccountGranter struct {
	ensureCalls  []string
	regrantCalls []string
	ensureErr    error
	regrantErr   error
}

func (f *fakeAccountGranter) EnsureAccount(_ context.Context, accountID string) error {
	f.ensureCalls = append(f.ensureCalls, accountID)
	return f.ensureErr
}

func (f *fakeAccountGranter) RegrantAccount(_ context.Context, accountID string) error {
	f.regrantCalls = append(f.regrantCalls, accountID)
	return f.regrantErr
}

var _ AccountGranter = (*fakeAccountGranter)(nil)

// newBackupTestAppliance builds a real *Appliance (fakeLauncher, embedded
// JetStream) seeded AVAILABLE at endpoint:port with no WithHostPort, so
// dsn() resolves straight to that endpoint the way TestConnect_
// NoHostPortDepsSkipsEnsure relies on for Connect.
func newBackupTestAppliance(t *testing.T, endpoint string, port int) *Appliance {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	masterKey := testMasterKey(t)
	appliance, err := NewAppliance(js, masterKey, &fakeLauncher{})
	require.NoError(t, err)
	seedAvailableAppliance(t, appliance, masterKey, endpoint, port)
	return appliance
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return out
}

func TestBackupObjectKey_ScopedUnderAccountPrefixAndTimestamped(t *testing.T) {
	at := time.Date(2026, 8, 18, 12, 30, 0, 0, time.UTC)
	key := backupObjectKey(backupTestAccount, at)
	assert.Equal(t, backupTestAccount+"/"+backupTestAccount+"-20260818T123000Z.sql.gz", key)
}

func TestBackupService_Backup_UploadsGzippedDumpUnderAccountKey(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := objectstore.NewMemoryObjectStore()
	dumper := &fakeDumper{dumpContent: []byte("-- pg_dump output for kb_" + backupTestAccount)}
	svc := NewBackupService(appliance, &fakeAccountGranter{}, store, dumper)

	out, err := svc.Backup(context.Background(), &BackupAccountRequest{AccountID: backupTestAccount}, "")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, backupTestAccount+"/", out.ObjectKey[:len(backupTestAccount)+1])
	assert.Equal(t, "kb_"+backupTestAccount, dumper.dumpSchema)
	assert.Equal(t, 1, dumper.dumpCalls)
	assert.NotZero(t, out.SizeBytes)

	got, err := store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(backupBucket),
		Key:    aws.String(out.ObjectKey),
	})
	require.NoError(t, err)
	data, err := io.ReadAll(got.Body)
	require.NoError(t, err)
	assert.Equal(t, dumper.dumpContent, gunzip(t, data))
}

func TestBackupService_Backup_InvalidAccountIDRejected(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	dumper := &fakeDumper{}
	svc := NewBackupService(appliance, &fakeAccountGranter{}, objectstore.NewMemoryObjectStore(), dumper)

	_, err := svc.Backup(context.Background(), &BackupAccountRequest{AccountID: "not-an-account"}, "")
	require.Error(t, err)
	assert.Equal(t, 0, dumper.dumpCalls, "dumper must never run for a rejected account id")
}

func TestBackupService_Backup_DumperErrorLeavesNoObjectUploaded(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := objectstore.NewMemoryObjectStore()
	dumper := &fakeDumper{dumpErr: errors.New("pg_dump: connection refused")}
	svc := NewBackupService(appliance, &fakeAccountGranter{}, store, dumper)

	_, err := svc.Backup(context.Background(), &BackupAccountRequest{AccountID: backupTestAccount}, "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "connection refused")
	assert.Equal(t, 0, store.Count())
}

// failingStore wraps MemoryObjectStore so tests can script EnsureBucket/
// PutObject failures the way a predastore outage would, without a live
// backend.
type failingStore struct {
	*objectstore.MemoryObjectStore

	ensureBucketErr error
	putObjectErr    error
}

func (f *failingStore) EnsureBucket(ctx context.Context, bucket string) error {
	if f.ensureBucketErr != nil {
		return f.ensureBucketErr
	}
	return f.MemoryObjectStore.EnsureBucket(ctx, bucket)
}

func (f *failingStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if f.putObjectErr != nil {
		return nil, f.putObjectErr
	}
	return f.MemoryObjectStore.PutObject(ctx, input)
}

var _ objectstore.ObjectStore = (*failingStore)(nil)

func TestBackupService_Backup_EnsureBucketErrorPropagates(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := &failingStore{
		MemoryObjectStore: objectstore.NewMemoryObjectStore(),
		ensureBucketErr:   errors.New("predastore: bucket quota exceeded"),
	}
	dumper := &fakeDumper{dumpContent: []byte("-- pg_dump output")}
	svc := NewBackupService(appliance, &fakeAccountGranter{}, store, dumper)

	_, err := svc.Backup(context.Background(), &BackupAccountRequest{AccountID: backupTestAccount}, "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "bucket quota exceeded")
}

func TestBackupService_Backup_PutObjectErrorPropagates(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := &failingStore{
		MemoryObjectStore: objectstore.NewMemoryObjectStore(),
		putObjectErr:      errors.New("predastore: connection reset"),
	}
	dumper := &fakeDumper{dumpContent: []byte("-- pg_dump output")}
	svc := NewBackupService(appliance, &fakeAccountGranter{}, store, dumper)

	_, err := svc.Backup(context.Background(), &BackupAccountRequest{AccountID: backupTestAccount}, "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "connection reset")
}

func TestBackupService_Backup_ApplianceNotAvailablePropagates(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	appliance, err := NewAppliance(js, testMasterKey(t), &fakeLauncher{})
	require.NoError(t, err)
	// Never seeded AVAILABLE: dsn() must refuse before the dumper ever runs.
	dumper := &fakeDumper{}
	svc := NewBackupService(appliance, &fakeAccountGranter{}, objectstore.NewMemoryObjectStore(), dumper)

	_, err = svc.Backup(context.Background(), &BackupAccountRequest{AccountID: backupTestAccount}, "")
	require.Error(t, err)
	assert.Equal(t, 0, dumper.dumpCalls)
}

// seedBackupObject uploads a gzipped SQL script under accountID's own
// object-key prefix, standing in for a prior Backup call.
func seedBackupObject(t *testing.T, store *objectstore.MemoryObjectStore, accountID, sql string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write([]byte(sql))
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	key := backupObjectKey(accountID, time.Now())
	require.NoError(t, store.EnsureBucket(context.Background(), backupBucket))
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(backupBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(buf.Bytes()),
	})
	require.NoError(t, err)
	return key
}

func TestBackupService_Restore_RunsPsqlThenRegrantsAccount(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := objectstore.NewMemoryObjectStore()
	key := seedBackupObject(t, store, backupTestAccount, "COPY kb_"+backupTestAccount+".idx_x FROM stdin;\n")
	dumper := &fakeDumper{}
	granter := &fakeAccountGranter{}
	svc := NewBackupService(appliance, granter, store, dumper)

	out, err := svc.Restore(context.Background(), &RestoreAccountRequest{AccountID: backupTestAccount, ObjectKey: key}, "")
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, 1, dumper.restoreCalls)
	assert.Contains(t, string(dumper.restoreContent), "COPY kb_"+backupTestAccount)
	assert.Equal(t, []string{backupTestAccount}, granter.ensureCalls)
	assert.Equal(t, []string{backupTestAccount}, granter.regrantCalls)
}

func TestBackupService_Restore_RejectsMismatchedAccountPrefix(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := objectstore.NewMemoryObjectStore()
	otherAccount := "000000000099"
	key := seedBackupObject(t, store, otherAccount, "-- not this account's dump")
	dumper := &fakeDumper{}
	granter := &fakeAccountGranter{}
	svc := NewBackupService(appliance, granter, store, dumper)

	_, err := svc.Restore(context.Background(), &RestoreAccountRequest{AccountID: backupTestAccount, ObjectKey: key}, "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "does not belong to account")
	assert.Equal(t, 0, dumper.restoreCalls)
	assert.Empty(t, granter.ensureCalls, "must not touch the account's schema on a rejected key")
}

func TestBackupService_Restore_MissingObjectKeyRejected(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	svc := NewBackupService(appliance, &fakeAccountGranter{}, objectstore.NewMemoryObjectStore(), &fakeDumper{})

	_, err := svc.Restore(context.Background(), &RestoreAccountRequest{AccountID: backupTestAccount}, "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "requires an object key")
}

func TestBackupService_Restore_ObjectNotFoundPropagates(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	svc := NewBackupService(appliance, &fakeAccountGranter{}, objectstore.NewMemoryObjectStore(), &fakeDumper{})

	_, err := svc.Restore(context.Background(), &RestoreAccountRequest{
		AccountID: backupTestAccount,
		ObjectKey: backupTestAccount + "/missing.sql.gz",
	}, "")
	require.Error(t, err)
}

func TestBackupService_Restore_PsqlErrorSkipsRegrant(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := objectstore.NewMemoryObjectStore()
	key := seedBackupObject(t, store, backupTestAccount, "-- broken sql")
	dumper := &fakeDumper{restoreErr: errors.New("psql: syntax error")}
	granter := &fakeAccountGranter{}
	svc := NewBackupService(appliance, granter, store, dumper)

	_, err := svc.Restore(context.Background(), &RestoreAccountRequest{AccountID: backupTestAccount, ObjectKey: key}, "")
	require.Error(t, err)
	assert.Empty(t, granter.ensureCalls, "a failed import must not be followed by a grant repair")
}

func TestBackupService_Restore_InvalidAccountIDRejected(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	dumper := &fakeDumper{}
	svc := NewBackupService(appliance, &fakeAccountGranter{}, objectstore.NewMemoryObjectStore(), dumper)

	_, err := svc.Restore(context.Background(), &RestoreAccountRequest{AccountID: "not-an-account", ObjectKey: "x/y.sql.gz"}, "")
	require.Error(t, err)
	assert.Equal(t, 0, dumper.restoreCalls, "dumper must never run for a rejected account id")
}

func TestBackupService_Restore_ApplianceNotAvailablePropagates(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	appliance, err := NewAppliance(js, testMasterKey(t), &fakeLauncher{})
	require.NoError(t, err)
	// Never seeded AVAILABLE: dsn() must refuse before the dumper ever runs.
	dumper := &fakeDumper{}
	svc := NewBackupService(appliance, &fakeAccountGranter{}, objectstore.NewMemoryObjectStore(), dumper)

	_, err = svc.Restore(context.Background(), &RestoreAccountRequest{AccountID: backupTestAccount, ObjectKey: backupTestAccount + "/x.sql.gz"}, "")
	require.Error(t, err)
	assert.Equal(t, 0, dumper.restoreCalls)
}

func TestBackupService_Restore_CorruptGzipObjectRejected(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := objectstore.NewMemoryObjectStore()
	require.NoError(t, store.EnsureBucket(context.Background(), backupBucket))
	key := backupTestAccount + "/not-gzip.sql.gz"
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(backupBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte("this is not a gzip stream")),
	})
	require.NoError(t, err)
	dumper := &fakeDumper{}
	svc := NewBackupService(appliance, &fakeAccountGranter{}, store, dumper)

	_, err = svc.Restore(context.Background(), &RestoreAccountRequest{AccountID: backupTestAccount, ObjectKey: key}, "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "open backup gzip stream")
	assert.Equal(t, 0, dumper.restoreCalls)
}

func TestBackupService_Restore_EnsureAccountErrorPropagates(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := objectstore.NewMemoryObjectStore()
	key := seedBackupObject(t, store, backupTestAccount, "-- sql")
	dumper := &fakeDumper{}
	granter := &fakeAccountGranter{ensureErr: errors.New("grant: schema locked")}
	svc := NewBackupService(appliance, granter, store, dumper)

	_, err := svc.Restore(context.Background(), &RestoreAccountRequest{AccountID: backupTestAccount, ObjectKey: key}, "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "schema locked")
	assert.Empty(t, granter.regrantCalls, "must not attempt regrant when the ensure-account repair fails")
}

func TestBackupService_Restore_RegrantAccountErrorPropagates(t *testing.T) {
	appliance := newBackupTestAppliance(t, "127.0.0.1", 5432)
	store := objectstore.NewMemoryObjectStore()
	key := seedBackupObject(t, store, backupTestAccount, "-- sql")
	dumper := &fakeDumper{}
	granter := &fakeAccountGranter{regrantErr: errors.New("grant: role missing")}
	svc := NewBackupService(appliance, granter, store, dumper)

	_, err := svc.Restore(context.Background(), &RestoreAccountRequest{AccountID: backupTestAccount, ObjectKey: key}, "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "role missing")
	assert.Equal(t, []string{backupTestAccount}, granter.ensureCalls, "ensure-account must still run before the failing regrant")
}
