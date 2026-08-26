package gateway_bedrock

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// Price is a resolved per-model price in integer micro-USD per million
// tokens (D12). Known is false when no price could be resolved at all — the
// caller MUST treat that distinctly from a resolved zero price. Self-hosted
// models resolve to a known zero (their real cost is GPU-hours, already
// accounted by the instance); a provider model with neither a KV override
// nor an in-tree default resolves to Known=false, and the two must never be
// mistaken for one another in a billing counter.
type Price struct {
	InputMicroUSDPerMillion  int64 `json:"inputMicroUsdPerMillion"`
	OutputMicroUSDPerMillion int64 `json:"outputMicroUsdPerMillion"`
	Known                    bool  `json:"known"`
}

// PriceResolver resolves modelID's KV-overridden price, if one has been set.
// ok is false when no override exists, in which case resolvePrice falls back
// to the catalog entry's in-tree default.
type PriceResolver interface {
	Resolve(ctx context.Context, modelID string) (price Price, ok bool, err error)
}

// resolvePrice determines entry's price: self-host is always a known zero: a
// KV override or in-tree default is the resolution order for everything
// else, and neither existing leaves the price unknown rather than free. store
// may be nil, in which case only the in-tree default is consulted.
func resolvePrice(ctx context.Context, store PriceResolver, entry catalogEntry) (Price, error) {
	if entry.Provider == tierSelfHost {
		return Price{Known: true}, nil
	}
	if store != nil {
		price, ok, err := store.Resolve(ctx, entry.ModelID)
		if err != nil {
			return Price{}, fmt.Errorf("resolve price override for %s: %w", entry.ModelID, err)
		}
		if ok {
			return price, nil
		}
	}
	if entry.PriceKnown {
		return Price{
			InputMicroUSDPerMillion:  entry.InputPriceMicroUSDPerMillion,
			OutputMicroUSDPerMillion: entry.OutputPriceMicroUSDPerMillion,
			Known:                    true,
		}, nil
	}
	return Price{}, nil
}

// bedrockPricesBucket is the cluster-replicated KV bucket holding per-model
// price overrides, keyed like bedrock-weights (base64url of the model ID,
// since model IDs contain ':').
const bedrockPricesBucket = "bedrock-prices"

// bedrockPricesHistory keeps one revision; a re-priced model overwrites in
// place.
const bedrockPricesHistory = 1

// PriceStore resolves per-model price overrides from the bedrock-prices
// JetStream KV bucket, mirroring WeightsStore's construction and lookup shape
// — both are per-model deployment data with an in-tree default plus a KV
// override (D4, D12).
type PriceStore struct {
	store *kvstore.Store[Price]
}

var _ PriceResolver = (*PriceStore)(nil)

// NewPriceStore constructs a PriceStore over the cluster's JetStream client,
// replicated across replicas nodes.
func NewPriceStore(js jetstream.JetStream, replicas int) *PriceStore {
	return &PriceStore{store: kvstore.New[Price](js, kvstore.Config{
		Name:     bedrockPricesBucket,
		History:  bedrockPricesHistory,
		Replicas: replicas,
		Missing:  "bedrock: price store has no JetStream client configured",
	})}
}

// Resolve returns modelID's KV-overridden price, if one has been set. Errors
// name the model ID, since the key itself is base64url.
func (s *PriceStore) Resolve(ctx context.Context, modelID string) (Price, bool, error) {
	price, _, err := s.store.Get(ctx, weightsKey(modelID))
	if errors.Is(err, kvstore.ErrNotFound) {
		return Price{}, false, nil
	}
	if err != nil {
		return Price{}, false, fmt.Errorf("price override for %s: %w", modelID, err)
	}
	return *price, true, nil
}

// PutPrice records price as modelID's overridden price, always Known
// (storing a KV override to mark a model unknown makes no sense — remove the
// override with DeletePrice instead to fall back to the in-tree default).
func (s *PriceStore) PutPrice(ctx context.Context, modelID string, price Price) error {
	price.Known = true
	if err := s.store.Set(ctx, weightsKey(modelID), &price); err != nil {
		return fmt.Errorf("price override for %s: %w", modelID, err)
	}
	return nil
}

// DeletePrice removes modelID's price override, falling resolution back to
// the catalog entry's in-tree default. Deleting an already-absent override is
// idempotent, not an error.
func (s *PriceStore) DeletePrice(ctx context.Context, modelID string) error {
	if err := s.store.Delete(ctx, weightsKey(modelID)); err != nil {
		return fmt.Errorf("price override for %s: %w", modelID, err)
	}
	return nil
}
