package function

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// ClaimTTL is how long a record still storing its code holds its name. It must
// exceed the gateway request deadline plus the longest permitted code write.
const ClaimTTL = 5 * time.Minute

// ErrNotFound means no record is bound to the name.
var ErrNotFound = errors.New("lambda function not found")

var (
	nameRE   = regexp.MustCompile(`^[a-zA-Z0-9-_]{1,64}$`)
	regionRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	idRE     = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

// ValidName reports whether name is a legal function name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

func badRegion() error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "region is invalid")
}

// MetaService is the daemon-side owner of function records. Every update is a
// compare-and-swap on the revision the caller read or was handed.
type MetaService struct {
	js  jetstream.JetStream
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]jetstream.KeyValue
}

// NewMetaService builds the service over a JetStream handle.
func NewMetaService(js jetstream.JetStream) *MetaService {
	return &MetaService{js: js, now: time.Now, buckets: make(map[string]jetstream.KeyValue)}
}

func (s *MetaService) bucket(ctx context.Context, accountID string) (jetstream.KeyValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kv, ok := s.buckets[accountID]; ok {
		return kv, nil
	}
	name := KVAccountBucket(accountID)
	kv, err := kvstore.GetOrCreateBucket(ctx, s.js, name, KVBucketAccountHistory)
	if err != nil {
		return nil, fmt.Errorf("lambda: open account bucket %s: %w", name, err)
	}
	s.buckets[accountID] = kv
	return kv, nil
}

func conflict(format string, args ...any) error {
	return awserrors.Errorf(awserrors.ErrorResourceConflictException, format, args...)
}

func decode(data []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, awserrors.Errorf(awserrors.ErrorServiceException, "function record is unreadable")
	}
	if rec.FormatVersion != FormatVersion {
		return Record{}, awserrors.Errorf(awserrors.ErrorServiceException,
			"function record has unsupported format version %d", rec.FormatVersion)
	}
	return rec, nil
}

// Create records the code upload, then claims the name with kv.Create: Pending,
// code storing, claim expiring after ClaimTTL. A name held only by an expired,
// still-storing claim is taken over once; the taken-over upload stays owed.
func (s *MetaService) Create(ctx context.Context, req *CreateRequest, accountID string) (*CreateResponse, error) {
	rec := req.Record
	if !regionRE.MatchString(req.Region) {
		return nil, badRegion()
	}
	if !ValidName(rec.FunctionName) || !idRE.MatchString(rec.FunctionID) || !idRE.MatchString(rec.Code.RevisionID) || accountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "function record is incomplete")
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	rec.FormatVersion = FormatVersion
	rec.AccountID, rec.Region = accountID, req.Region
	rec.State = StatePending
	rec.StateReason, rec.StateReasonCode = "", ""
	rec.Code = Code{RevisionID: rec.Code.RevisionID, State: CodeStoring}
	rec.ClaimExpiresAt = now.Add(ClaimTTL)
	rec.CreatedAt, rec.LastModified = now, now
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	key := KVFunctionKey(req.Region, rec.FunctionName)

	// The upload is recorded before the claim exists, so no copy can be written
	// for a claim that has no upload record.
	upKey := KVUploadKey(req.Region, rec.FunctionID, rec.Code.RevisionID)
	upData, err := json.Marshal(Upload{
		FormatVersion: FormatVersion, AccountID: accountID, Region: req.Region, FunctionName: rec.FunctionName,
		FunctionID: rec.FunctionID, RevisionID: rec.Code.RevisionID, State: UploadOwed, CreatedAt: now,
	})
	if err != nil {
		return nil, err
	}
	upRev, err := kv.Create(ctx, upKey, upData)
	if err != nil {
		return nil, fmt.Errorf("lambda: record code upload: %w", err)
	}

	rev, err := kv.Create(ctx, key, data)
	if err == nil {
		return &CreateResponse{Revision: rev}, nil
	}
	if !errors.Is(err, jetstream.ErrKeyExists) {
		return nil, fmt.Errorf("lambda: create function record: %w", err)
	}

	exists := conflict("Function already exist: %s", rec.FunctionName)
	entry, err := kv.Get(ctx, key)
	if err != nil {
		return nil, s.refused(ctx, kv, upKey, upRev, exists)
	}
	old, err := decode(entry.Value())
	if err != nil || old.Code.State != CodeStoring || !s.now().After(old.ClaimExpiresAt) {
		return nil, s.refused(ctx, kv, upKey, upRev, exists)
	}
	if err := kv.Delete(ctx, key, jetstream.LastRevision(entry.Revision())); err != nil {
		return nil, s.refused(ctx, kv, upKey, upRev, exists)
	}
	slog.WarnContext(ctx, "Lambda: took over an expired function name claim",
		"function_name", rec.FunctionName, "old_function_id", old.FunctionID)
	rev, err = kv.Create(ctx, key, data)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, s.refused(ctx, kv, upKey, upRev, exists)
		}
		return nil, fmt.Errorf("lambda: create function record: %w", err)
	}
	return &CreateResponse{Revision: rev}, nil
}

// refused drops the upload record of a create that certainly holds no claim:
// the gateway writes code only after Create succeeds. A failed drop leaves the
// upload owed, which is safe.
func (s *MetaService) refused(ctx context.Context, kv jetstream.KeyValue, upKey string, upRev uint64, err error) error {
	if derr := kv.Delete(ctx, upKey, jetstream.LastRevision(upRev)); derr != nil {
		slog.WarnContext(ctx, "Lambda: upload record of a refused create was not dropped", "key", upKey, "err", derr)
	}
	return err
}

// checkUpload returns the owed upload record for rec. Any other record, or
// none, refuses the caller; nothing is rewritten, because the state of a record
// this owner cannot vouch for is unknown.
func checkUpload(ctx context.Context, kv jetstream.KeyValue, rec Record) (Upload, uint64, error) {
	refuse := func(why string, args ...any) (Upload, uint64, error) {
		slog.WarnContext(ctx, "Lambda: code upload record refused; its state is unknown",
			"function_id", rec.FunctionID, "revision_id", rec.Code.RevisionID, "reason", fmt.Sprintf(why, args...))
		return Upload{}, 0, awserrors.Errorf(awserrors.ErrorServiceException, "function code upload is not tracked")
	}
	entry, err := kv.Get(ctx, KVUploadKey(rec.Region, rec.FunctionID, rec.Code.RevisionID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return refuse("missing")
	}
	if err != nil {
		return Upload{}, 0, fmt.Errorf("lambda: get code upload record: %w", err)
	}
	var up Upload
	if err := json.Unmarshal(entry.Value(), &up); err != nil {
		return refuse("unreadable")
	}
	if up.FormatVersion != FormatVersion {
		return refuse("format version %d", up.FormatVersion)
	}
	if up.AccountID != rec.AccountID || up.Region != rec.Region || up.FunctionName != rec.FunctionName ||
		up.FunctionID != rec.FunctionID || up.RevisionID != rec.Code.RevisionID {
		return refuse("identity mismatch")
	}
	if up.State != UploadOwed {
		return refuse("state %s", up.State)
	}
	return up, entry.Revision(), nil
}

// attach marks the checked upload as named by the function record at revision.
// KV has no multi-key write, so a change since the check fails this CAS, and
// the record is then left as found, its state unknown.
func attach(ctx context.Context, kv jetstream.KeyValue, up Upload, upRev, revision uint64) {
	up.State, up.AttachedRevision = UploadAttached, revision
	data, err := json.Marshal(up)
	if err == nil {
		_, err = kv.Update(ctx, KVUploadKey(up.Region, up.FunctionID, up.RevisionID), data, upRev)
	}
	if err != nil {
		slog.WarnContext(ctx, "Lambda: upload record not marked attached; its state is unknown",
			"function_id", up.FunctionID, "revision_id", up.RevisionID, "err", err)
	}
}

// Get returns the record, or Found false when the name is unbound.
func (s *MetaService) Get(ctx context.Context, req *GetRequest, accountID string) (*GetResponse, error) {
	if !regionRE.MatchString(req.Region) {
		return nil, badRegion()
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	entry, err := kv.Get(ctx, KVFunctionKey(req.Region, req.FunctionName))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return &GetResponse{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lambda: get function record: %w", err)
	}
	rec, err := decode(entry.Value())
	if err != nil {
		return nil, err
	}
	return &GetResponse{Found: true, Record: rec, Revision: entry.Revision()}, nil
}

// readStoring loads the record and checks it is the one the caller created: the
// revision matches and the code is still storing.
func (s *MetaService) readStoring(ctx context.Context, kv jetstream.KeyValue, region, name string, revision uint64) (Record, error) {
	entry, err := kv.Get(ctx, KVFunctionKey(region, name))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("lambda: get function record: %w", err)
	}
	if entry.Revision() != revision {
		return Record{}, conflict("Function was modified by another request: %s", name)
	}
	rec, err := decode(entry.Value())
	if err != nil {
		return Record{}, err
	}
	if rec.Code.State != CodeStoring {
		return Record{}, conflict("Function code is not being stored: %s", name)
	}
	return rec, nil
}

// CodeStored marks the code stored. It is refused, leaving both records unchanged,
// when the revision moved, the code is not storing, the claim has expired, or
// the upload record is not the owed one this create wrote.
func (s *MetaService) CodeStored(ctx context.Context, req *CodeStoredRequest, accountID string) (*CodeStoredResponse, error) {
	if !regionRE.MatchString(req.Region) {
		return nil, badRegion()
	}
	if !digestRE.MatchString(req.SHA256) || req.Size < 0 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "code digest is invalid")
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, err := s.readStoring(ctx, kv, req.Region, req.FunctionName, req.Revision)
	if errors.Is(err, ErrNotFound) {
		return nil, awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "Function not found: %s", req.FunctionName)
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if now.After(rec.ClaimExpiresAt) {
		return nil, conflict("Function creation claim expired: %s", req.FunctionName)
	}
	up, upRev, err := checkUpload(ctx, kv, rec)
	if err != nil {
		return nil, err
	}
	rec.Code.State, rec.Code.SHA256, rec.Code.Size = CodeStored, req.SHA256, req.Size
	rec.State, rec.StateReasonCode, rec.StateReason = StatePending, ReasonCodeCreating, ReasonCreating
	rec.LastModified = now.UTC()
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	rev, err := kv.Update(ctx, KVFunctionKey(req.Region, req.FunctionName), data, req.Revision)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, conflict("Function was modified by another request: %s", req.FunctionName)
		}
		return nil, fmt.Errorf("lambda: update function record: %w", err)
	}
	attach(ctx, kv, up, upRev, rev)
	return &CodeStoredResponse{Record: rec, Revision: rev}, nil
}

// RollbackCreate frees the name of a create whose own code write failed. It
// deletes only a record still storing at exactly the revision create returned;
// an already-absent name is success. The upload stays owed: the write may have landed.
func (s *MetaService) RollbackCreate(ctx context.Context, req *RollbackCreateRequest, accountID string) (*RollbackCreateResponse, error) {
	if !regionRE.MatchString(req.Region) {
		return nil, badRegion()
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	_, err = s.readStoring(ctx, kv, req.Region, req.FunctionName, req.Revision)
	if errors.Is(err, ErrNotFound) {
		return &RollbackCreateResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := kv.Delete(ctx, KVFunctionKey(req.Region, req.FunctionName), jetstream.LastRevision(req.Revision)); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, conflict("Function was modified by another request: %s", req.FunctionName)
		}
		return nil, fmt.Errorf("lambda: delete function record: %w", err)
	}
	return &RollbackCreateResponse{}, nil
}
