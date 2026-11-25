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

// ConsoleBranding is the branding store, the gateway's copy of it and the
// [console] gate. A nil one answers both methods with InternalError.
type ConsoleBranding struct {
	Enabled bool
	Store   consolebranding.Store
	Cache   *consolebranding.Cache
	Now     func() time.Time
}

const brandingDisabledMsg = "console branding is not enabled on this cluster; set branding_enabled = true under [console] in spinifex.toml on every node"

// getConsoleBranding works with the gate off, so an operator can see what is
// stored before turning it on.
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
	return rec.Response(d.Branding.Enabled, rev), nil
}

// putConsoleBranding replaces the record in one revision-checked write, so a
// save or a reset never lands in part.
func (d Deps) putConsoleBranding(ctx context.Context, caller dispatch.Actor, body []byte) (any, error) {
	b := d.Branding
	if b == nil {
		slog.ErrorContext(ctx, "PutConsoleBranding: branding store not available")
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if !b.Enabled {
		return nil, awserrors.Errorf(awserrors.ErrorUnsupportedOperation, "%s", brandingDisabledMsg)
	}
	var req operatorv1.PutConsoleBrandingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidRequest, "request body is not a branding record")
	}

	stored, rev, err := b.Store.Get(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "PutConsoleBranding: failed to read record", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if req.Revision != rev {
		return nil, stalePrecondition()
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

	newRev, err := b.Store.Put(ctx, next, rev)
	if errors.Is(err, consolebranding.ErrStaleRevision) {
		return nil, stalePrecondition()
	}
	if err != nil {
		slog.ErrorContext(ctx, "PutConsoleBranding: failed to store record", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if b.Cache != nil {
		b.Cache.Set(&next, newRev)
	}
	slog.InfoContext(ctx, "Console branding updated", "updatedBy", next.UpdatedBy,
		"revision", newRev, "changed", consolebranding.ChangedFields(stored, next))
	return next.Response(true, newRev), nil
}

func stalePrecondition() error {
	return awserrors.Errorf(awserrors.ErrorPreconditionFailedException,
		"the branding has changed since it was read; reload it and save again")
}
