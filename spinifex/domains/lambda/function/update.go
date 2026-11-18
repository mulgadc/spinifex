package function

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
)

// readServing loads the record a code update names and checks it is still the
// one the caller read: same revision and function, Active, with stored code.
func readServing(ctx context.Context, kv jetstream.KeyValue, region, name, functionID string, revision uint64) (Record, error) {
	entry, err := kv.Get(ctx, KVFunctionKey(region, name))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Record{}, awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "Function not found: %s", name)
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
	if rec.FunctionID != functionID {
		return Record{}, conflict("Function was modified by another request: %s", name)
	}
	if rec.State != StateActive || rec.Code.State != CodeStored {
		return Record{}, conflict("The operation cannot be performed at this time. The function is currently in the following state: %s", rec.State)
	}
	return rec, nil
}

func validUpdate(region, name, functionID, revisionID, accountID string) error {
	if !regionRE.MatchString(region) {
		return badRegion()
	}
	if !ValidName(name) || !idRE.MatchString(functionID) || !idRE.MatchString(revisionID) || accountID == "" {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "code update request is incomplete")
	}
	return nil
}

// BeginCodeUpdate records an owed upload for a new code copy of the function
// at the caller's revision. The function record is left unchanged, so invokes
// keep running the current copy while the new one is written.
func (s *MetaService) BeginCodeUpdate(ctx context.Context, req *CodeUpdateBeginRequest, accountID string) (*CodeUpdateBeginResponse, error) {
	if err := validUpdate(req.Region, req.FunctionName, req.FunctionID, req.RevisionID, accountID); err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, err := readServing(ctx, kv, req.Region, req.FunctionName, req.FunctionID, req.Revision)
	if err != nil {
		return nil, err
	}
	if rec.Code.RevisionID == req.RevisionID {
		return nil, conflict("Function code revision is already in use: %s", req.FunctionName)
	}
	data, err := json.Marshal(Upload{
		FormatVersion: FormatVersion, AccountID: accountID, Region: req.Region, FunctionName: req.FunctionName,
		FunctionID: req.FunctionID, RevisionID: req.RevisionID, State: UploadOwed, CreatedAt: s.now().UTC(),
	})
	if err != nil {
		return nil, err
	}
	if _, err := kv.Create(ctx, KVUploadKey(req.Region, req.FunctionID, req.RevisionID), data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, conflict("Function code revision is already in use: %s", req.FunctionName)
		}
		return nil, fmt.Errorf("lambda: record code upload: %w", err)
	}
	return &CodeUpdateBeginResponse{}, nil
}

// CodeUpdated points the function at its new stored copy by compare-and-swap at
// the revision the update began from; only the code and last-modified time
// change. The previous copy and its upload record are kept.
func (s *MetaService) CodeUpdated(ctx context.Context, req *CodeUpdatedRequest, accountID string) (*CodeUpdatedResponse, error) {
	if err := validUpdate(req.Region, req.FunctionName, req.FunctionID, req.RevisionID, accountID); err != nil {
		return nil, err
	}
	if !digestRE.MatchString(req.SHA256) || req.Size <= 0 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "code digest is invalid")
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, err := readServing(ctx, kv, req.Region, req.FunctionName, req.FunctionID, req.Revision)
	if err != nil {
		return nil, err
	}
	next := rec
	next.Code = Code{RevisionID: req.RevisionID, SHA256: req.SHA256, Size: req.Size, State: CodeStored}
	up, upRev, err := checkUpload(ctx, kv, next)
	if err != nil {
		return nil, err
	}
	next.LastModified = s.now().UTC()
	data, err := json.Marshal(next)
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
	return &CodeUpdatedResponse{Record: next, Revision: rev}, nil
}
