package gateway_bedrock

import (
	"context"
	"fmt"
)

// stagedOpenAccessResolver wraps an inner AccessResolver so a staged
// self-host model is granted to every account without an explicit grant:
// import is the grant. An explicit inner grant (and the GlobalAccountID
// system bypass it already honors) still wins, so the per-account seam stays
// live for a later isolated/paid tier. Provider-tier models are unaffected —
// they still need an explicit inner grant.
type stagedOpenAccessResolver struct {
	inner AccessResolver
}

var _ AccessResolver = (*stagedOpenAccessResolver)(nil)

// NewStagedOpenAccessResolver wraps inner so a staged self-host model is
// granted to every account without an explicit grant, per import-is-the-grant.
func NewStagedOpenAccessResolver(inner AccessResolver) AccessResolver {
	return stagedOpenAccessResolver{inner: inner}
}

// Granted reports true when inner already grants accountID modelID, or when
// modelID is a self-host catalog entry with a staged weights snapshot. A
// weights-resolve error is a fault, not a false, and propagates rather than
// silently denying access.
func (r stagedOpenAccessResolver) Granted(ctx context.Context, accountID, modelID string) (bool, error) {
	granted, err := r.inner.Granted(ctx, accountID, modelID)
	if err != nil {
		return false, err
	}
	if granted {
		return true, nil
	}

	entry, ok := lookupCatalogEntry(modelID)
	if !ok || entry.Provider != tierSelfHost {
		return false, nil
	}

	_, staged, err := currentWeightsResolver().Resolve(ctx, modelID)
	if err != nil {
		return false, fmt.Errorf("resolve weights for %s: %w", modelID, err)
	}
	return staged, nil
}
