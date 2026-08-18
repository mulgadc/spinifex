package handlers_ochrevector

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// SubjectBackupAccount and SubjectRestoreAccount are the operator-only NATS
// subjects for account-scoped pgvector schema backup/restore (D2-adjacent):
// reached solely via `spx admin ochre vector backup`/`restore`, never
// through the tenant-facing awsgw surface.
const (
	SubjectBackupAccount  = "ochre.appliance.backupAccount"
	SubjectRestoreAccount = "ochre.appliance.restoreAccount"
)

// backupBucket holds every account's pgvector schema backup, one predastore
// bucket with objects keyed per account and timestamp. Backup is an explicit
// operator action (never automatic on teardown), so a stray or malicious
// tenant request can never trigger a dump: only the operator CLI reaches
// this subject.
const backupBucket = "ochre-vector-backups"

// backupObjectKeyLayout timestamps a backup object so successive backups of
// the same account never collide and sort lexically newest-last.
const backupObjectKeyLayout = "20060102T150405Z"

// BackupAccountRequest names the account to back up. Carried in the payload
// rather than derived from the caller's own NATS header identity: the
// operator CLI always calls in as the global account, but must be able to
// target any tenant account's schema, not just its own.
type BackupAccountRequest struct {
	AccountID string `json:"accountId"`
}

// BackupAccountResponse reports where the backup landed, so the operator can
// hand ObjectKey straight to a later restore call.
type BackupAccountResponse struct {
	ObjectKey string `json:"objectKey"`
	SizeBytes int64  `json:"sizeBytes"`
}

// RestoreAccountRequest names the account and the backup object to restore
// into it. AccountID is payload-carried for the same reason as
// BackupAccountRequest's.
type RestoreAccountRequest struct {
	AccountID string `json:"accountId"`
	ObjectKey string `json:"objectKey"`
}

// RestoreAccountResponse has no fields: a successful restore has nothing
// further to report beyond a nil error.
type RestoreAccountResponse struct{}

// PgDumper is the seam to the pg_dump/psql binaries this package shells out
// to: the real implementation execs them against the appliance DSN, tests
// substitute a fake so the object-key/gzip/streaming plumbing around it is
// provable without a live Postgres or the binaries installed.
type PgDumper interface {
	// Dump streams a plain-SQL dump of schema at dsn to w. The dump is
	// self-cleaning (DROP ... IF EXISTS before each CREATE) and carries no
	// owner/privilege statements, so importing it never depends on which
	// role happens to run the restore.
	Dump(ctx context.Context, dsn, schema string, w io.Writer) error
	// Restore runs the SQL read from r against the database at dsn.
	Restore(ctx context.Context, dsn string, r io.Reader) error
}

// ExecPgDumper is the real PgDumper: it shells out to the pg_dump and psql
// binaries, which must be present on the node's PATH.
type ExecPgDumper struct{}

var _ PgDumper = ExecPgDumper{}

// Dump runs pg_dump scoped to schema, --clean/--if-exists so replaying the
// output against an existing (or freshly EnsureAccount'd) schema is safe,
// and --no-owner/--no-privileges so the output never references a role that
// may not exist yet on the restore target.
func (ExecPgDumper) Dump(ctx context.Context, dsn, schema string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "pg_dump",
		"--dbname="+dsn,
		"--schema="+schema,
		"--no-owner",
		"--no-privileges",
		"--clean",
		"--if-exists",
	)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ochrevector: pg_dump schema %s: %w: %s", schema, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Restore runs psql against dsn, reading the SQL script from r and stopping
// on the first error rather than continuing past a partially-applied
// restore.
func (ExecPgDumper) Restore(ctx context.Context, dsn string, r io.Reader) error {
	cmd := exec.CommandContext(ctx, "psql",
		"--dbname="+dsn,
		"--set=ON_ERROR_STOP=1",
		"--quiet",
	)
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ochrevector: psql restore: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// AccountGranter is the post-restore schema/privilege repair surface
// BackupService needs: EnsureAccount recreates the schema-level grant a
// dump's DROP/CREATE SCHEMA wipes, RegrantAccount then covers the tables and
// sequences the dump itself recreated with no owner. *pgxBackend satisfies
// it structurally; exported so daemon wiring can type-assert a VectorBackend
// against it without this package's help.
type AccountGranter interface {
	EnsureAccount(ctx context.Context, accountID string) error
	RegrantAccount(ctx context.Context, accountID string) error
}

var _ AccountGranter = (*pgxBackend)(nil)

// applianceDSNResolver is the DSN-resolution surface BackupService needs
// from *Appliance; narrowed to the one method so a fake appliance never has
// to satisfy the whole singleton-orchestration type in tests.
type applianceDSNResolver interface {
	dsn(ctx context.Context) (string, error)
}

var _ applianceDSNResolver = (*Appliance)(nil)

// BackupService performs account-scoped pg_dump/restore of the appliance's
// per-account schema to/from predastore S3 (D3). Backup is an explicit
// operator action, never automatic; Restore into an existing schema
// overwrites it cleanly via the dump's own --clean/--if-exists statements,
// then repairs the account role's grants that a schema-level DROP wipes.
type BackupService struct {
	Appliance applianceDSNResolver
	Backend   AccountGranter
	Store     objectstore.ObjectStore
	Dumper    PgDumper
	// Now stands in for time.Now in tests, so a backup's object key is
	// deterministic.
	Now func() time.Time
}

// NewBackupService constructs a BackupService over its dependencies.
func NewBackupService(appliance applianceDSNResolver, backend AccountGranter, store objectstore.ObjectStore, dumper PgDumper) *BackupService {
	return &BackupService{Appliance: appliance, Backend: backend, Store: store, Dumper: dumper, Now: time.Now}
}

// backupObjectKey scopes a backup object under accountID's own key prefix,
// timestamped so successive backups never collide.
func backupObjectKey(accountID string, at time.Time) string {
	return fmt.Sprintf("%s/%s-%s.sql.gz", accountID, accountID, at.UTC().Format(backupObjectKeyLayout))
}

// Backup dumps accountID's schema, gzips it to a temp file (bounding memory
// to the gzip buffer rather than the whole dump), then uploads it to
// predastore under an account-scoped, timestamped key.
func (s *BackupService) Backup(ctx context.Context, req *BackupAccountRequest, _ string) (*BackupAccountResponse, error) {
	accountID := req.AccountID
	if err := validateAccountID(accountID); err != nil {
		return nil, err
	}
	dsn, err := s.Appliance.dsn(ctx)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: resolve appliance dsn for backup: %w", err)
	}

	tmp, err := os.CreateTemp("", "ochre-vector-backup-*.sql.gz")
	if err != nil {
		return nil, fmt.Errorf("ochrevector: create backup temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	defer tmp.Close()

	gz := gzip.NewWriter(tmp)
	if err := s.Dumper.Dump(ctx, dsn, schemaName(accountID), gz); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("ochrevector: flush backup gzip stream for account %s: %w", accountID, err)
	}

	info, err := tmp.Stat()
	if err != nil {
		return nil, fmt.Errorf("ochrevector: stat backup temp file: %w", err)
	}
	size := info.Size()
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("ochrevector: rewind backup temp file: %w", err)
	}

	if err := s.Store.EnsureBucket(ctx, backupBucket); err != nil {
		return nil, fmt.Errorf("ochrevector: ensure backup bucket: %w", err)
	}

	key := backupObjectKey(accountID, s.Now())
	if _, err := s.Store.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(backupBucket),
		Key:           aws.String(key),
		Body:          tmp,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/gzip"),
	}); err != nil {
		return nil, fmt.Errorf("ochrevector: upload backup object %s: %w", key, err)
	}

	return &BackupAccountResponse{ObjectKey: key, SizeBytes: size}, nil
}

// Restore fetches objectKey from predastore, gunzips it straight into psql
// against the appliance, then repairs the account role's grants (the dump
// itself carries none) so query/ingest access resumes exactly as before the
// backup. Idempotent: rerunning against the same or a fresh schema is safe.
func (s *BackupService) Restore(ctx context.Context, req *RestoreAccountRequest, _ string) (*RestoreAccountResponse, error) {
	accountID := req.AccountID
	if err := validateAccountID(accountID); err != nil {
		return nil, err
	}
	if req.ObjectKey == "" {
		return nil, errors.New("ochrevector: restore requires an object key")
	}
	// Guards against restoring the wrong account's data into accountID by
	// operator mistake: every backup key is minted under its own account
	// prefix (backupObjectKey), so a mismatch here is always a typo, never
	// a legitimate cross-account restore.
	if !strings.HasPrefix(req.ObjectKey, accountID+"/") {
		return nil, fmt.Errorf("ochrevector: object key %s does not belong to account %s", req.ObjectKey, accountID)
	}

	dsn, err := s.Appliance.dsn(ctx)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: resolve appliance dsn for restore: %w", err)
	}

	out, err := s.Store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(backupBucket), Key: aws.String(req.ObjectKey)})
	if err != nil {
		return nil, fmt.Errorf("ochrevector: fetch backup object %s: %w", req.ObjectKey, err)
	}
	defer out.Body.Close()

	gz, err := gzip.NewReader(out.Body)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: open backup gzip stream %s: %w", req.ObjectKey, err)
	}
	defer gz.Close()

	if err := s.Dumper.Restore(ctx, dsn, gz); err != nil {
		return nil, err
	}

	// The dump's own DROP/CREATE SCHEMA wipes the schema-level USAGE/CREATE
	// grant EnsureAccount made; repeating it here is idempotent and restores
	// exactly that grant, then RegrantAccount covers the tables/sequences
	// pg_dump recreated with no owner.
	if err := s.Backend.EnsureAccount(ctx, accountID); err != nil {
		return nil, fmt.Errorf("ochrevector: re-ensure account %s after restore: %w", accountID, err)
	}
	if err := s.Backend.RegrantAccount(ctx, accountID); err != nil {
		return nil, fmt.Errorf("ochrevector: regrant account %s after restore: %w", accountID, err)
	}

	return &RestoreAccountResponse{}, nil
}
