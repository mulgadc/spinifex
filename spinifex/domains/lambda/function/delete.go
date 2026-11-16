package function

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
)

// Delete records the function's cleanup obligation, then unbinds its name by
// compare-and-swap at the revision the caller read. Code copies and upload
// records are retained; nothing here removes them.
func (s *MetaService) Delete(ctx context.Context, req *DeleteRequest, accountID string) (*DeleteResponse, error) {
	if !regionRE.MatchString(req.Region) {
		return nil, badRegion()
	}
	if !ValidName(req.FunctionName) || !idRE.MatchString(req.FunctionID) || accountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "delete request is incomplete")
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	key := KVFunctionKey(req.Region, req.FunctionName)
	entry, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "Function not found: %s", req.FunctionName)
	}
	if err != nil {
		return nil, fmt.Errorf("lambda: get function record: %w", err)
	}
	rec, err := decode(entry.Value())
	if err != nil {
		return nil, err
	}
	if entry.Revision() != req.Revision || rec.FunctionID != req.FunctionID {
		return nil, conflict("Function was modified by another request: %s", req.FunctionName)
	}
	if err := s.recordDeletion(ctx, kv, accountID, rec); err != nil {
		return nil, err
	}
	if err := kv.Delete(ctx, key, jetstream.LastRevision(req.Revision)); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, conflict("Function was modified by another request: %s", req.FunctionName)
		}
		return nil, fmt.Errorf("lambda: delete function record: %w", err)
	}
	return &DeleteResponse{}, nil
}

// recordDeletion writes the obligation for rec. One already present, from an
// earlier attempt whose outcome was uncertain, is accepted only for the same
// function; anything else refuses the delete.
func (s *MetaService) recordDeletion(ctx context.Context, kv jetstream.KeyValue, accountID string, rec Record) error {
	want := Deletion{
		FormatVersion: FormatVersion, AccountID: accountID, Region: rec.Region,
		FunctionName: rec.FunctionName, FunctionID: rec.FunctionID, RecordedAt: s.now().UTC(),
	}
	data, err := json.Marshal(want)
	if err != nil {
		return err
	}
	key := KVDeletedKey(rec.Region, rec.FunctionID)
	_, err = kv.Create(ctx, key, data)
	if err == nil {
		return nil
	}
	if !errors.Is(err, jetstream.ErrKeyExists) {
		return fmt.Errorf("lambda: record function deletion: %w", err)
	}
	entry, err := kv.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("lambda: read function deletion: %w", err)
	}
	var have Deletion
	if err := json.Unmarshal(entry.Value(), &have); err != nil {
		return fmt.Errorf("lambda: function deletion record is unreadable: %w", err)
	}
	if have.FormatVersion != FormatVersion || have.AccountID != accountID || have.Region != rec.Region ||
		have.FunctionName != rec.FunctionName || have.FunctionID != rec.FunctionID {
		return fmt.Errorf("lambda: function deletion record %s names another function", key)
	}
	return nil
}
