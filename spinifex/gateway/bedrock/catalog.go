package gateway_bedrock

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// Tier flags on catalogEntry.Provider. "self-host" models run on Spinifex GPU
// compute; "provider:<vendor>" models are served by calling the vendor's own
// API and require a resolvable per-account (or platform-default) credential.
const (
	tierSelfHost    = "self-host"
	providerPrefix  = "provider:"
	vendorAnthropic = "anthropic"
	modelARNFormat  = "arn:aws:bedrock:*::foundation-model/%s"
)

// catalogEntry is one static catalog record. Model IDs mirror AWS exactly so
// existing SDK code and configs are drop-in.
type catalogEntry struct {
	ModelID                    string
	ModelName                  string
	ProviderName               string
	Provider                   string // "self-host" or "provider:<vendor>"
	InputModalities            []string
	OutputModalities           []string
	ResponseStreamingSupported bool
	InferenceTypesSupported    []string
	CustomizationsSupported    []string
}

// catalog is the static Phase-1 model set: one self-hosted open model and one
// Anthropic-direct model. Later phases extend this list.
var catalog = []catalogEntry{
	{
		ModelID:                    "meta.llama3-70b-instruct-v1:0",
		ModelName:                  "Llama 3 70B Instruct",
		ProviderName:               "Meta",
		Provider:                   tierSelfHost,
		InputModalities:            []string{"TEXT"},
		OutputModalities:           []string{"TEXT"},
		ResponseStreamingSupported: false,
		InferenceTypesSupported:    []string{"ON_DEMAND"},
	},
	{
		ModelID:                    "anthropic.claude-3-5-sonnet-20240620-v1:0",
		ModelName:                  "Claude 3.5 Sonnet",
		ProviderName:               "Anthropic",
		Provider:                   providerPrefix + vendorAnthropic,
		InputModalities:            []string{"TEXT", "IMAGE"},
		OutputModalities:           []string{"TEXT"},
		ResponseStreamingSupported: false,
		InferenceTypesSupported:    []string{"ON_DEMAND"},
	},
}

// CatalogModelIDs returns every model ID the platform knows about, ignoring
// grants and credential tiering. It exists for administration — granting an
// account the whole catalog — and must not be used to answer an account's own
// ListFoundationModels, which is grant-filtered.
func CatalogModelIDs() []string {
	ids := make([]string, 0, len(catalog))
	for _, entry := range catalog {
		ids = append(ids, entry.ModelID)
	}
	return ids
}

// CredentialResolver resolves accountID's usable provider credential for
// vendor: a per-account key, else an optional platform default. key is only
// meaningful when ok is true.
type CredentialResolver interface {
	Resolve(ctx context.Context, accountID, vendor string) (key string, ok bool, err error)
}

// tieredCatalog returns the catalog entries advertised to accountID: those the
// account holds a grant on, and among those, provider entries only where
// resolver finds a usable credential. Access is checked first, so a model the
// account was never granted stays hidden even when a platform-default
// credential would otherwise serve it.
func tieredCatalog(ctx context.Context, accountID string, resolver CredentialResolver, access AccessResolver) []catalogEntry {
	var out []catalogEntry
	for _, entry := range catalog {
		granted, err := access.Granted(ctx, accountID, entry.ModelID)
		if err != nil || !granted {
			continue
		}
		if entry.Provider == tierSelfHost {
			out = append(out, entry)
			continue
		}
		vendor, ok := strings.CutPrefix(entry.Provider, providerPrefix)
		if !ok {
			continue
		}
		_, resolvable, err := resolver.Resolve(ctx, accountID, vendor)
		if err != nil || !resolvable {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func (e catalogEntry) toSummary() *bedrock.FoundationModelSummary {
	return &bedrock.FoundationModelSummary{
		ModelArn:                   aws.String(modelARN(e.ModelID)),
		ModelId:                    aws.String(e.ModelID),
		ModelName:                  aws.String(e.ModelName),
		ProviderName:               aws.String(e.ProviderName),
		InputModalities:            aws.StringSlice(e.InputModalities),
		OutputModalities:           aws.StringSlice(e.OutputModalities),
		ResponseStreamingSupported: aws.Bool(e.ResponseStreamingSupported),
		InferenceTypesSupported:    aws.StringSlice(e.InferenceTypesSupported),
		CustomizationsSupported:    aws.StringSlice(e.CustomizationsSupported),
	}
}

func (e catalogEntry) toDetails() *bedrock.FoundationModelDetails {
	return &bedrock.FoundationModelDetails{
		ModelArn:                   aws.String(modelARN(e.ModelID)),
		ModelId:                    aws.String(e.ModelID),
		ModelName:                  aws.String(e.ModelName),
		ProviderName:               aws.String(e.ProviderName),
		InputModalities:            aws.StringSlice(e.InputModalities),
		OutputModalities:           aws.StringSlice(e.OutputModalities),
		ResponseStreamingSupported: aws.Bool(e.ResponseStreamingSupported),
		InferenceTypesSupported:    aws.StringSlice(e.InferenceTypesSupported),
		CustomizationsSupported:    aws.StringSlice(e.CustomizationsSupported),
	}
}

func modelARN(modelID string) string {
	return fmt.Sprintf(modelARNFormat, modelID)
}

// lookupCatalogEntry finds a catalog entry by exact modelId, ignoring both
// tier gating and access grants. Callers on a request path must use
// grantedCatalogEntry instead; this is the raw table lookup underneath it.
func lookupCatalogEntry(modelID string) (catalogEntry, bool) {
	for _, entry := range catalog {
		if entry.ModelID == modelID {
			return entry, true
		}
	}
	return catalogEntry{}, false
}

// grantedCatalogEntry resolves modelID to its catalog entry and checks
// accountID's grant on it. Every runtime path shares this one gate so the four
// routers cannot drift apart on who may invoke what.
//
// An unknown model and an ungranted one are deliberately distinguishable here
// (ResourceNotFoundException vs AccessDeniedException) because the caller has
// already been told the model exists by its own catalog listing; the describe
// path collapses both to ResourceNotFoundException instead, where that is not
// true.
func grantedCatalogEntry(ctx context.Context, accountID, modelID string, access AccessResolver) (catalogEntry, error) {
	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		return catalogEntry{}, errors.New(awserrors.ErrorResourceNotFoundException)
	}
	granted, err := access.Granted(ctx, accountID, modelID)
	if err != nil {
		return catalogEntry{}, err
	}
	if !granted {
		return catalogEntry{}, errors.New(awserrors.ErrorAccessDeniedException)
	}
	return entry, nil
}

// ListFoundationModels returns the catalog visible to accountID: models it
// holds a grant on, with provider entries further filtered to those whose
// credential resolves.
func ListFoundationModels(ctx context.Context, accountID string, resolver CredentialResolver, access AccessResolver, _ *bedrock.ListFoundationModelsInput) (*bedrock.ListFoundationModelsOutput, error) {
	entries := tieredCatalog(ctx, accountID, resolver, access)
	summaries := make([]*bedrock.FoundationModelSummary, 0, len(entries))
	for _, entry := range entries {
		summaries = append(summaries, entry.toSummary())
	}
	return &bedrock.ListFoundationModelsOutput{ModelSummaries: summaries}, nil
}

// GetFoundationModel looks up a single model by exact modelId, gated by the
// caller's grant. An ungranted model is reported as ResourceNotFoundException
// rather than AccessDeniedException so describe agrees with list: a model the
// account cannot see does not exist as far as this API is concerned, and the
// error does not confirm the model's existence to an account probing for it.
func GetFoundationModel(ctx context.Context, accountID string, modelID string, access AccessResolver) (*bedrock.GetFoundationModelOutput, error) {
	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}
	granted, err := access.Granted(ctx, accountID, modelID)
	if err != nil {
		return nil, err
	}
	if !granted {
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}
	return &bedrock.GetFoundationModelOutput{ModelDetails: entry.toDetails()}, nil
}
