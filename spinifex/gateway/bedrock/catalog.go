package gateway_bedrock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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
	// familyMeta is the self-host model family served by the Llama invoke
	// adapter. Any other self-host family is unhandled and refused explicitly
	// rather than mis-served through Llama's native request schema.
	familyMeta = "meta"
	// familyTEI is the self-host family for the TEI engine (EMBEDDING and
	// RERANK entries). No InvokeAdapter serves it yet, so it is unhandled and
	// refused the same as any other family besides familyMeta.
	familyTEI = "tei"
	// coServeGroupOchreDemo is the one populated co-serve group: the demo
	// LLM plus its embedder and reranker, admitted and launched as a bundle.
	coServeGroupOchreDemo = "ochre-demo-bundle"
)

// catalogEntry is one static catalog record. Model IDs mirror AWS exactly so
// existing SDK code and configs are drop-in.
//
// MinVRAMMiB, InstanceType and VLLMArgs are the in-tree serving spec for
// self-host entries; the actual staged weights artifact is deployment-local
// state resolved from KV instead (see WeightsResolver).
//
// InputPriceMicroUSDPerMillion/OutputPriceMicroUSDPerMillion and PriceKnown
// are the in-tree default price (D12): integer micro-USD per million tokens,
// same shape as the serving spec above. PriceKnown distinguishes an
// explicitly-priced provider entry from one nobody has priced yet — a bare
// zero value is ambiguous with a genuine $0 price, so resolvePrice never
// reads the two int fields without checking it first. Self-host entries leave
// all three at their zero value: resolvePrice special-cases tierSelfHost to
// always return a known zero price rather than consulting these fields.
//
// CoServeGroup ties self-host entries that co-reside in one serving VM: an
// empty value is a standalone model (a bundle of one), a shared non-empty
// value means the entries admit and launch together as a single GPU claim.
type catalogEntry struct {
	ModelID                       string
	ModelName                     string
	ProviderName                  string
	Provider                      string // "self-host" or "provider:<vendor>"
	Family                        string // self-host only; selects the invoke adapter (see familyMeta, familyTEI)
	CoServeGroup                  string // self-host only; shared co-serve group id, empty = standalone
	HFRepo                        string // self-host only; canonical Hugging Face repo 'weights pull' defaults to
	HFRevision                    string // self-host only; pinned HF commit/tag 'weights pull' defaults to, empty = main
	InputModalities               []string
	OutputModalities              []string
	ResponseStreamingSupported    bool
	InferenceTypesSupported       []string
	CustomizationsSupported       []string
	MinVRAMMiB                    int      // self-host only; 0 for provider entries
	InstanceType                  string   // self-host only; system-instance type a launcher boots
	VLLMArgs                      []string // self-host only; extra vLLM server args this model needs
	InputPriceMicroUSDPerMillion  int64    // provider entries only; meaningless unless PriceKnown
	OutputPriceMicroUSDPerMillion int64    // provider entries only; meaningless unless PriceKnown
	PriceKnown                    bool     // provider entries only; self-host is always known-zero
}

// catalog is the static model set: two self-hosted open models and one
// Anthropic-direct model. Later phases extend this list.
var catalog = []catalogEntry{
	{
		ModelID:                    "meta.llama3-2-1b-instruct-v1:0",
		ModelName:                  "Llama 3.2 1B Instruct",
		ProviderName:               "Meta",
		Provider:                   tierSelfHost,
		Family:                     familyMeta,
		CoServeGroup:               coServeGroupOchreDemo,
		HFRepo:                     "meta-llama/Llama-3.2-1B-Instruct",
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
		ModelID:                    "meta.llama3-2-3b-instruct-v1:0",
		ModelName:                  "Llama 3.2 3B Instruct",
		ProviderName:               "Meta",
		Provider:                   tierSelfHost,
		Family:                     familyMeta,
		HFRepo:                     "meta-llama/Llama-3.2-3B-Instruct",
		InputModalities:            []string{"TEXT"},
		OutputModalities:           []string{"TEXT"},
		ResponseStreamingSupported: false,
		InferenceTypesSupported:    []string{"ON_DEMAND"},
		// 3.21B params at bf16 is roughly 6124 MiB of weights, so on an 8188 MiB
		// card the budget is tight: --enforce-eager skips CUDA graph capture to
		// free several hundred MiB, and max-model-len is half the 1B entry's.
		MinVRAMMiB:   7168,
		InstanceType: "g5.xlarge",
		VLLMArgs: []string{
			"--dtype=bfloat16", "--max-model-len=4096", "--gpu-memory-utilization=0.92",
			"--max-num-seqs=8", "--enforce-eager",
		},
	},
	{
		ModelID:      "nomic-embed-text-v1.5",
		ModelName:    "Nomic Embed Text v1.5",
		ProviderName: "Nomic AI",
		Provider:     tierSelfHost,
		Family:       familyTEI,
		CoServeGroup: coServeGroupOchreDemo,
		HFRepo:       "nomic-ai/nomic-embed-text-v1.5",
		// Pin a pre-v5 revision: current main's config.json carries both
		// max_position_embeddings and n_positions, which TEI rejects as a
		// duplicate field. This revision keeps the 768-dim, 8k-context model.
		HFRevision:                 "e5cf08a",
		InputModalities:            []string{"TEXT"},
		OutputModalities:           []string{"EMBEDDING"},
		ResponseStreamingSupported: false,
		InferenceTypesSupported:    []string{"ON_DEMAND"},
		MinVRAMMiB:                 512,
		InstanceType:               "g5.xlarge",
		// Mean pooling stated explicitly: the flat weights layout omits
		// 1_Pooling/config.json, so TEI would otherwise default to CLS.
		VLLMArgs: []string{"--pooling", "mean"},
	},
	{
		ModelID:                    "bge-base-en-v1.5",
		ModelName:                  "BGE Base EN v1.5",
		ProviderName:               "BAAI",
		Provider:                   tierSelfHost,
		Family:                     familyTEI,
		CoServeGroup:               "",
		HFRepo:                     "BAAI/bge-base-en-v1.5",
		InputModalities:            []string{"TEXT"},
		OutputModalities:           []string{"EMBEDDING"},
		ResponseStreamingSupported: false,
		InferenceTypesSupported:    []string{"ON_DEMAND"},
		// 768-dim, 512-token context; standard BERT, TEI-native. Standalone
		// alternative embedder to nomic.
		MinVRAMMiB:   512,
		InstanceType: "g5.xlarge",
	},
	{
		ModelID:                    "bge-reranker-v2-m3",
		ModelName:                  "BGE Reranker v2 M3",
		ProviderName:               "BAAI",
		Provider:                   tierSelfHost,
		Family:                     familyTEI,
		CoServeGroup:               coServeGroupOchreDemo,
		HFRepo:                     "BAAI/bge-reranker-v2-m3",
		InputModalities:            []string{"TEXT"},
		OutputModalities:           []string{"RERANK"},
		ResponseStreamingSupported: false,
		InferenceTypesSupported:    []string{"ON_DEMAND"},
		MinVRAMMiB:                 1200,
		InstanceType:               "g5.xlarge",
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
		// List pricing at launch: $3/MTok input, $15/MTok output.
		InputPriceMicroUSDPerMillion:  3_000_000,
		OutputPriceMicroUSDPerMillion: 15_000_000,
		PriceKnown:                    true,
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
// account holds a grant on, and among those, self-host entries only where a
// weights snapshot resolves and provider entries only where resolver finds a
// usable credential. Access is checked first, so a model the account was never
// granted stays hidden even when a platform-default credential would otherwise
// serve it. A weights resolve error is an internal fault, not a servability
// verdict, and aborts the whole list rather than silently thinning it out.
func tieredCatalog(ctx context.Context, accountID string, resolver CredentialResolver, access AccessResolver) ([]catalogEntry, error) {
	var out []catalogEntry
	for _, entry := range catalog {
		granted, err := access.Granted(ctx, accountID, entry.ModelID)
		if err != nil || !granted {
			continue
		}
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

// CoServeGroupVRAMMiB resolves modelID's co-serve group and returns the
// summed MinVRAMMiB floor across every entry sharing it, plus every member's
// model ID (modelID included). A model with no group is a bundle of one:
// totalMinVRAMMiB is just its own floor and members holds only itself.
func CoServeGroupVRAMMiB(modelID string) (totalMinVRAMMiB int, members []string, found bool) {
	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		return 0, nil, false
	}
	if entry.CoServeGroup == "" {
		return entry.MinVRAMMiB, []string{entry.ModelID}, true
	}
	for _, e := range catalog {
		if e.CoServeGroup != entry.CoServeGroup {
			continue
		}
		totalMinVRAMMiB += e.MinVRAMMiB
		members = append(members, e.ModelID)
	}
	return totalMinVRAMMiB, members, true
}

// FamilyMeta and FamilyTEI are the exported aliases of familyMeta/familyTEI,
// for callers outside this package that dispatch on a co-serve member's own
// family (the launcher's engine choice, the userData port assignment).
const (
	FamilyMeta = familyMeta
	FamilyTEI  = familyTEI
)

// defaultVLLMGPUMemoryUtilization is the ceiling assumed for a vLLM member
// with no explicit --gpu-memory-utilization of its own, mirroring vLLM's own
// server default.
const defaultVLLMGPUMemoryUtilization = 0.9

// CoServeMember is one model co-served on a bundle's shared VM. VLLMArgs is
// the member's own launch args, already bundle-scaled for a vLLM member (see
// scaleVLLMGPUMemoryUtilization) — the launcher uses it as-is, it does not
// re-derive the group's VRAM split.
type CoServeMember struct {
	ModelID    string
	Family     string
	MinVRAMMiB int
	VLLMArgs   []string
}

// CoServeGroupSpec is modelID's resolved co-serve group: every member sharing
// the VM, the group's summed VRAM floor (matching CoServeGroupVRAMMiB's own
// sum), and the instance type to boot. A standalone model resolves to a
// bundle of one, GroupID equal to its own ModelID.
type CoServeGroupSpec struct {
	GroupID         string
	Members         []CoServeMember
	TotalMinVRAMMiB int
	InstanceType    string
}

// scaleVLLMGPUMemoryUtilization replaces args' --gpu-memory-utilization (or
// assumes vLLM's own default when args carries none) with a bundle-scaled
// value: the solo ceiling times this member's share of the group's summed
// VRAM floor. Without this a co-served vLLM process would claim its
// solo-launch share of the whole card, leaving no headroom for the TEI
// members sharing it.
func scaleVLLMGPUMemoryUtilization(args []string, memberMinVRAMMiB, totalMinVRAMMiB int) []string {
	if totalMinVRAMMiB <= 0 {
		return args
	}
	solo := defaultVLLMGPUMemoryUtilization
	out := make([]string, 0, len(args)+1)
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--gpu-memory-utilization="); ok {
			if parsed, err := strconv.ParseFloat(v, 64); err == nil {
				solo = parsed
			}
			continue
		}
		out = append(out, a)
	}
	share := float64(memberMinVRAMMiB) / float64(totalMinVRAMMiB)
	out = append(out, fmt.Sprintf("--gpu-memory-utilization=%.2f", solo*share))
	return out
}

// LookupCoServeGroup resolves modelID to the co-serve group it launches with.
// A standalone self-host entry (no CoServeGroup) resolves to a bundle of one
// carrying its own VLLMArgs unchanged, so the launch path needs no special
// case for today's single-model behaviour. found and selfHost mirror
// LookupServingSpec: found is false for an unknown model id; selfHost is
// false for a known provider entry.
func LookupCoServeGroup(modelID string) (spec CoServeGroupSpec, found, selfHost bool) {
	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		return CoServeGroupSpec{}, false, false
	}
	if entry.Provider != tierSelfHost {
		return CoServeGroupSpec{}, true, false
	}

	groupEntries := []catalogEntry{entry}
	groupID := entry.ModelID
	if entry.CoServeGroup != "" {
		groupEntries = nil
		groupID = entry.CoServeGroup
		for _, e := range catalog {
			if e.CoServeGroup == entry.CoServeGroup {
				groupEntries = append(groupEntries, e)
			}
		}
	}

	total := 0
	for _, e := range groupEntries {
		total += e.MinVRAMMiB
	}

	members := make([]CoServeMember, 0, len(groupEntries))
	for _, e := range groupEntries {
		args := e.VLLMArgs
		if e.Family == familyMeta && len(groupEntries) > 1 {
			args = scaleVLLMGPUMemoryUtilization(e.VLLMArgs, e.MinVRAMMiB, total)
		}
		members = append(members, CoServeMember{
			ModelID:    e.ModelID,
			Family:     e.Family,
			MinVRAMMiB: e.MinVRAMMiB,
			VLLMArgs:   args,
		})
	}

	return CoServeGroupSpec{
		GroupID:         groupID,
		Members:         members,
		TotalMinVRAMMiB: total,
		InstanceType:    entry.InstanceType,
	}, true, true
}

// ServingSpec is the subset of a self-host catalog entry needed both by
// external callers (the 'ochre weights stage' CLI, validating a model ID
// before staging weights) and by the daemon-side launcher (placing and
// booting the serving VM), without exposing catalogEntry's AWS-shaped fields.
type ServingSpec struct {
	ModelID      string
	MinVRAMMiB   int
	InstanceType string
	VLLMArgs     []string
	HFRepo       string
	HFRevision   string
}

// LookupServingSpec returns modelID's serving spec. found reports whether
// modelID exists in the catalog at all; selfHost reports whether it is a
// self-host entry (as opposed to a provider entry) when found is true. A
// caller wanting the old found-and-servable behaviour checks found && selfHost.
func LookupServingSpec(modelID string) (spec ServingSpec, found, selfHost bool) {
	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		return ServingSpec{}, false, false
	}
	if entry.Provider != tierSelfHost {
		return ServingSpec{}, true, false
	}
	return ServingSpec{
		ModelID:      entry.ModelID,
		MinVRAMMiB:   entry.MinVRAMMiB,
		InstanceType: entry.InstanceType,
		VLLMArgs:     entry.VLLMArgs,
		HFRepo:       entry.HFRepo,
		HFRevision:   entry.HFRevision,
	}, true, true
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
// holds a grant on, with self-host entries further filtered to those whose
// weights snapshot resolves and provider entries to those whose credential
// resolves.
func ListFoundationModels(ctx context.Context, accountID string, resolver CredentialResolver, access AccessResolver, _ *bedrock.ListFoundationModelsInput) (*bedrock.ListFoundationModelsOutput, error) {
	entries, err := tieredCatalog(ctx, accountID, resolver, access)
	if err != nil {
		return nil, err
	}
	summaries := make([]*bedrock.FoundationModelSummary, 0, len(entries))
	for _, entry := range entries {
		summaries = append(summaries, entry.toSummary())
	}
	return &bedrock.ListFoundationModelsOutput{ModelSummaries: summaries}, nil
}

// GetFoundationModel looks up a single model by exact modelId, gated by the
// caller's grant. An ungranted model, and a self-host model with no resolvable
// weights snapshot, are both reported as ResourceNotFoundException rather than
// AccessDeniedException so describe agrees with list: a model the account
// cannot see does not exist as far as this API is concerned, and the error does
// not confirm the model's existence to an account probing for it. A weights
// resolve error is an internal fault, not a not-found verdict, and is surfaced
// (and logged) as such.
func GetFoundationModel(ctx context.Context, accountID string, modelID string, access AccessResolver) (*bedrock.GetFoundationModelOutput, error) {
	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}
	// Grant first: an ungranted account must not learn whether the model is
	// servable, only that it is not there.
	granted, err := access.Granted(ctx, accountID, modelID)
	if err != nil {
		return nil, err
	}
	if !granted {
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
