package gateway_bedrock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
//
// MinVRAMMiB, InstanceType and VLLMArgs are the in-tree serving spec for
// self-host entries; the actual staged weights artifact is deployment-local
// state resolved from KV instead (see WeightsResolver).
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
	MinVRAMMiB                 int      // self-host only; 0 for provider entries
	InstanceType               string   // self-host only; system-instance type a launcher boots
	VLLMArgs                   []string // self-host only; extra vLLM server args this model needs
}

// catalog is the static Phase-1 model set: one self-hosted open model and one
// Anthropic-direct model. Later phases extend this list.
var catalog = []catalogEntry{
	{
		ModelID:                    "meta.llama3-2-1b-instruct-v1:0",
		ModelName:                  "Llama 3.2 1B Instruct",
		ProviderName:               "Meta",
		Provider:                   tierSelfHost,
		InputModalities:            []string{"TEXT"},
		OutputModalities:           []string{"TEXT"},
		ResponseStreamingSupported: false,
		InferenceTypesSupported:    []string{"ON_DEMAND"},
		// MinVRAMMiB is the admission-gate floor; --gpu-memory-utilization
		// caps vLLM's own pool to roughly the same figure (8188 MiB * 0.6 ≈
		// 4913 MiB) so the two stay consistent rather than drifting apart.
		MinVRAMMiB:   5120,
		InstanceType: "g5.xlarge",
		VLLMArgs:     []string{"--dtype=bfloat16", "--max-model-len=8192", "--gpu-memory-utilization=0.6"},
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

// CredentialResolver resolves accountID's usable provider credential for
// vendor: a per-account key, else an optional platform default. key is only
// meaningful when ok is true.
type CredentialResolver interface {
	Resolve(ctx context.Context, accountID, vendor string) (key string, ok bool, err error)
}

// tieredCatalog returns the catalog entries advertised to accountID:
// self-host entries only where a weights snapshot resolves, provider entries
// only when resolver finds a usable credential. A resolve error (as opposed
// to a clean not-found) is an internal fault, not a servability verdict, and
// aborts the whole list rather than silently thinning it out.
func tieredCatalog(ctx context.Context, accountID string, resolver CredentialResolver) ([]catalogEntry, error) {
	var out []catalogEntry
	for _, entry := range catalog {
		if entry.Provider == tierSelfHost {
			_, resolvable, err := currentWeightsResolver().Resolve(ctx, entry.ModelID)
			if err != nil {
				slog.Error("bedrock: weights resolve failed", "model", entry.ModelID, "err", err)
				return nil, fmt.Errorf("resolve weights for %s: %w", entry.ModelID, err)
			}
			if resolvable {
				out = append(out, entry)
			}
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
	return out, nil
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

// lookupCatalogEntry finds a catalog entry by exact modelId, ignoring tier
// gating (used by the runtime router, which returns its own error class).
func lookupCatalogEntry(modelID string) (catalogEntry, bool) {
	for _, entry := range catalog {
		if entry.ModelID == modelID {
			return entry, true
		}
	}
	return catalogEntry{}, false
}

// ListFoundationModels returns the deployment-tiered catalog: self-host
// entries where a weights snapshot resolves, provider entries where a
// credential resolves.
func ListFoundationModels(ctx context.Context, accountID string, resolver CredentialResolver, _ *bedrock.ListFoundationModelsInput) (*bedrock.ListFoundationModelsOutput, error) {
	entries, err := tieredCatalog(ctx, accountID, resolver)
	if err != nil {
		return nil, err
	}
	summaries := make([]*bedrock.FoundationModelSummary, 0, len(entries))
	for _, entry := range entries {
		summaries = append(summaries, entry.toSummary())
	}
	return &bedrock.ListFoundationModelsOutput{ModelSummaries: summaries}, nil
}

// GetFoundationModel looks up a single model by exact modelId. Unknown
// models, and self-host models with no resolvable weights snapshot, return
// ResourceNotFoundException. A weights resolve error is an internal fault,
// not a not-found verdict, and is surfaced (and logged) as such.
func GetFoundationModel(ctx context.Context, _ string, modelID string) (*bedrock.GetFoundationModelOutput, error) {
	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}
	if entry.Provider == tierSelfHost {
		_, resolvable, err := currentWeightsResolver().Resolve(ctx, entry.ModelID)
		if err != nil {
			slog.Error("bedrock: weights resolve failed", "model", entry.ModelID, "err", err)
			return nil, fmt.Errorf("resolve weights for %s: %w", entry.ModelID, err)
		}
		if !resolvable {
			return nil, errors.New(awserrors.ErrorResourceNotFoundException)
		}
	}
	return &bedrock.GetFoundationModelOutput{ModelDetails: entry.toDetails()}, nil
}
