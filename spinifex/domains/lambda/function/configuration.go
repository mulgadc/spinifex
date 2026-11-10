package function

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
)

// UpdateConfiguration replaces the function's settings by compare-and-swap at
// the revision the caller read; identity, code and state are unchanged.
func (s *MetaService) UpdateConfiguration(ctx context.Context, req *ConfigurationUpdatedRequest, accountID string) (*ConfigurationUpdatedResponse, error) {
	if !regionRE.MatchString(req.Region) {
		return nil, badRegion()
	}
	cfg := req.Configuration
	if !ValidName(req.FunctionName) || !idRE.MatchString(req.FunctionID) || accountID == "" ||
		cfg.Role == "" || cfg.Handler == "" || cfg.Timeout < 1 || cfg.MemorySize < 1 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "configuration update request is incomplete")
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
	next.Role, next.Handler, next.Description = cfg.Role, cfg.Handler, cfg.Description
	next.Timeout, next.MemorySize, next.EnvCipher = cfg.Timeout, cfg.MemorySize, cfg.EnvCipher
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
	return &ConfigurationUpdatedResponse{Record: next, Revision: rev}, nil
}
