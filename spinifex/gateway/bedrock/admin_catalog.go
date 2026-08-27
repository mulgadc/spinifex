package gateway_bedrock

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Availability values an AdminCatalogEntry carries. AvailabilityAvailable
// means the model is servable to accountID today; the other three are the
// same reasons tieredCatalog computes and collapses to omission on the
// tenant-facing ListFoundationModels path.
const (
	AvailabilityAvailable = "available"
	AvailabilityUngranted = "ungranted"
	AvailabilityNoWeights = "no-weights-staged"
	// #nosec G101 -- an availability-reason label, not a credential value.
	AvailabilityNoCredential = "no-credential"
)

// AdminCatalogEntry is one catalog model as an operator sees it: every field
// ListFoundationModels exposes plus the ops metadata AWS's wire shape cannot
// carry (VRAM floor, instance type, co-serve group) and the specific
// availability reason tieredCatalog would otherwise collapse to omission.
type AdminCatalogEntry struct {
	ModelID                       string   `json:"modelId"`
	ModelName                     string   `json:"modelName"`
	Family                        string   `json:"family"`
	InputModalities               []string `json:"inputModalities"`
	OutputModalities              []string `json:"outputModalities"`
	ResponseStreamingSupported    bool     `json:"responseStreamingSupported"`
	InputPriceMicroUSDPerMillion  int64    `json:"inputPriceMicroUsdPerMillion"`
	OutputPriceMicroUSDPerMillion int64    `json:"outputPriceMicroUsdPerMillion"`
	PriceKnown                    bool     `json:"priceKnown"`
	MinVRAMMiB                    int      `json:"minVramMib"`
	InstanceType                  string   `json:"instanceType"`
	CoServeGroup                  string   `json:"coServeGroup"`
	Availability                  string   `json:"availability"`
}

// AdminCatalogList is the ListOchreCatalog action's response envelope.
type AdminCatalogList struct {
	Entries []AdminCatalogEntry `json:"entries"`
}

// toAdminEntry projects e into its admin-read shape under the given
// availability reason.
func (e catalogEntry) toAdminEntry(availability string) AdminCatalogEntry {
	return AdminCatalogEntry{
		ModelID:                       e.ModelID,
		ModelName:                     e.ModelName,
		Family:                        e.Family,
		InputModalities:               e.InputModalities,
		OutputModalities:              e.OutputModalities,
		ResponseStreamingSupported:    e.ResponseStreamingSupported,
		InputPriceMicroUSDPerMillion:  e.InputPriceMicroUSDPerMillion,
		OutputPriceMicroUSDPerMillion: e.OutputPriceMicroUSDPerMillion,
		PriceKnown:                    e.PriceKnown,
		MinVRAMMiB:                    e.MinVRAMMiB,
		InstanceType:                  e.InstanceType,
		CoServeGroup:                  e.CoServeGroup,
		Availability:                  availability,
	}
}

// availabilityReason runs the same three checks tieredCatalog uses to decide
// whether to include entry for accountID, but returns which one failed
// instead of a bool: an operator-only view is allowed to say why, where the
// tenant-facing catalog is not.
func availabilityReason(ctx context.Context, accountID string, entry catalogEntry, resolver CredentialResolver, access AccessResolver) (string, error) {
	granted, err := access.Granted(ctx, accountID, entry.ModelID)
	if err != nil {
		return "", err
	}
	if !granted {
		return AvailabilityUngranted, nil
	}

	if entry.Provider == tierSelfHost {
		_, resolvable, err := currentWeightsResolver().Resolve(ctx, entry.ModelID)
		if err != nil {
			slog.Error("bedrock: weights resolve failed", "model", entry.ModelID, "err", err)
			return "", fmt.Errorf("resolve weights for %s: %w", entry.ModelID, err)
		}
		if !resolvable {
			return AvailabilityNoWeights, nil
		}
		return AvailabilityAvailable, nil
	}

	vendor, ok := strings.CutPrefix(entry.Provider, providerPrefix)
	if !ok {
		return AvailabilityNoCredential, nil
	}
	_, resolvable, err := resolver.Resolve(ctx, accountID, vendor)
	if err != nil {
		return "", err
	}
	if !resolvable {
		return AvailabilityNoCredential, nil
	}
	return AvailabilityAvailable, nil
}

// AdminCatalog returns every catalog entry, unfiltered, with its
// availability computed for accountID: the honest operator read Open
// Decision #1 calls for, as opposed to ListFoundationModels which omits
// what accountID cannot use rather than say why. Callers must scope this to
// an operator-only surface — the reason it returns is exactly what
// grantedCatalogEntry and GetFoundationModel deliberately withhold from a
// tenant to avoid confirming a model's existence to a probing account.
func AdminCatalog(ctx context.Context, accountID string, resolver CredentialResolver, access AccessResolver) ([]AdminCatalogEntry, error) {
	out := make([]AdminCatalogEntry, 0, len(catalog))
	for _, entry := range catalog {
		reason, err := availabilityReason(ctx, accountID, entry, resolver, access)
		if err != nil {
			return nil, err
		}
		out = append(out, entry.toAdminEntry(reason))
	}
	return out, nil
}
