package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/operator/consolebranding"
)

// ConsoleBranding is the branding store and the gateway's copy of it. A nil
// one answers both methods with InternalError.
type ConsoleBranding struct {
	Store consolebranding.Store
	Cache *consolebranding.Cache
	Now   func() time.Time
}

// getConsoleBranding returns the stored record, without image bytes.
func (d Deps) getConsoleBranding(ctx context.Context, body []byte) (any, error) {
	if d.Branding == nil {
		slog.ErrorContext(ctx, "GetConsoleBranding: branding store not available")
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if err := json.Unmarshal(body, &struct{}{}); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidRequest)
	}
	rec, rev, err := d.Branding.Store.Get(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "GetConsoleBranding: failed to read record", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return rec.Response(rev), nil
}

// putConsoleBranding replaces the record in one revision-checked write, so a
// save or a reset never lands in part.
func (d Deps) putConsoleBranding(ctx context.Context, caller dispatch.Actor, body []byte) (any, error) {
	b := d.Branding
	if b == nil {
		slog.ErrorContext(ctx, "PutConsoleBranding: branding store not available")
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	var req operatorv1.PutConsoleBrandingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidRequest, "request body is not a branding record")
	}

	stored, _, err := b.Store.Get(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "PutConsoleBranding: failed to read record", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	next, err := consolebranding.Apply(stored, req)
	if ve, ok := errors.AsType[*consolebranding.ValidationError](err); ok {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s", ve.Error())
	}
	if err != nil {
		slog.ErrorContext(ctx, "PutConsoleBranding: failed to build record", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	next.UpdatedBy = actorName(caller)
	next.UpdatedAt = now().UTC().Format(time.RFC3339)

	newRev, err := b.Store.Put(ctx, next)
	if err != nil {
		slog.ErrorContext(ctx, "PutConsoleBranding: failed to store record", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if b.Cache != nil {
		b.Cache.Set(&next, newRev)
	}
	slog.InfoContext(ctx, "Console branding updated", "updatedBy", next.UpdatedBy,
		"revision", newRev, "changed", consolebranding.ChangedFields(stored, next))
	return next.Response(newRev), nil
}
