package gateway_bedrock

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// bedrockGuardrailBucket is the cluster-replicated KV bucket holding
// guardrail control-plane records.
const bedrockGuardrailBucket = "bedrock-guardrails"

// bedrockGuardrailHistory keeps one revision per key: a guardrail (DRAFT plus
// its numbered snapshots) is one JSON document mutated in place, not a series.
const bedrockGuardrailHistory = 1

// guardrailDraftVersion is the literal AWS uses for the mutable working copy
// of a guardrail, as opposed to an immutable numbered snapshot ("1", "2", ...).
const guardrailDraftVersion = "DRAFT"

// guardrailView is the configured shape shared by the mutable DRAFT and every
// immutable numbered snapshot: everything that came from a Create/Update
// call, as opposed to the identity/status fields that live only on the
// top-level record.
type guardrailView struct {
	Name                       string                                             `json:"name"`
	Description                string                                             `json:"description,omitempty"`
	BlockedInputMessaging      string                                             `json:"blockedInputMessaging"`
	BlockedOutputsMessaging    string                                             `json:"blockedOutputsMessaging"`
	WordPolicy                 *bedrock.GuardrailWordPolicyConfig                 `json:"wordPolicy,omitempty"`
	SensitiveInformationPolicy *bedrock.GuardrailSensitiveInformationPolicyConfig `json:"sensitiveInformationPolicy,omitempty"`
	ContentPolicy              *bedrock.GuardrailContentPolicyConfig              `json:"contentPolicy,omitempty"`
	TopicPolicy                *bedrock.GuardrailTopicPolicyConfig                `json:"topicPolicy,omitempty"`
	ContextualGroundingPolicy  *bedrock.GuardrailContextualGroundingPolicyConfig  `json:"contextualGroundingPolicy,omitempty"`
	CreatedAt                  time.Time                                          `json:"createdAt"`
	UpdatedAt                  time.Time                                          `json:"updatedAt"`
}

// GuardrailVersionSnapshot is one immutable numbered version, frozen from the
// DRAFT at the moment CreateGuardrailVersion was called. Later mutation of the
// DRAFT never reaches an already-created snapshot.
type GuardrailVersionSnapshot struct {
	guardrailView

	Version string `json:"version"`
}

// GuardrailRecord is the gateway control-plane state for one guardrail: the
// mutable DRAFT (guardrailView) plus every immutable numbered version taken
// from it. Policy configs are stored verbatim from the create/update input so
// the filter engine and inference enforcement (later stages) can read them
// back unchanged.
type GuardrailRecord struct {
	guardrailView

	ARN         string `json:"arn"`
	GuardrailID string `json:"guardrailId"`
	AccountID   string `json:"accountId"`
	// Status is READY as soon as Create succeeds: v1's policies are all
	// deterministic (string/regex match), so there is no async compile step
	// the way a classifier-backed guardrail would need.
	Status string `json:"status"`
	// Versions maps a numbered version string ("1", "2", ...) to its frozen
	// snapshot. NextVersion is the last version number minted, so a retried
	// CreateGuardrailVersion after a partial failure still advances.
	Versions    map[string]GuardrailVersionSnapshot `json:"versions,omitempty"`
	NextVersion int                                 `json:"nextVersion"`
}

// GuardrailStore persists GuardrailRecords in the bedrock-guardrails
// JetStream KV bucket, mirroring the ProvisionedStore/LoggingConfigStore
// gateway-direct-KV pattern.
type GuardrailStore struct {
	store *kvstore.Store[GuardrailRecord]
	// region is baked in at construction, like ProvisionedStore.region, so
	// ARN building/parsing needs no extra parameter threaded through the
	// fixed-arity route table.
	region string
}

// NewGuardrailStore constructs a GuardrailStore over js, replicated across
// replicas nodes, minting ARNs for region.
func NewGuardrailStore(js jetstream.JetStream, replicas int, region string) *GuardrailStore {
	return &GuardrailStore{
		store: kvstore.New[GuardrailRecord](js, kvstore.Config{
			Name:     bedrockGuardrailBucket,
			History:  bedrockGuardrailHistory,
			Replicas: replicas,
			Missing:  "bedrock: guardrail store has no JetStream client configured",
		}),
		region: region,
	}
}

// guardrailKey scopes every stored record to its owning account, so List's
// prefix scan is the only thing that ever needs to see across ids, and a
// foreign account's raw-id guess can never collide with another tenant's key.
func guardrailKey(accountID, id string) string {
	return accountID + "/" + id
}

// get reads accountID's record for id, or (zero, false, nil) if it does not
// exist.
func (s *GuardrailStore) get(ctx context.Context, accountID, id string) (GuardrailRecord, bool, error) {
	rec, _, found, err := s.getRevision(ctx, guardrailKey(accountID, id))
	return rec, found, err
}

// errGuardrailNotFound reports a guardrail-specific ResourceNotFoundException.
// awserrors' default wording for that code ("could not resolve the foundation
// model") is misleading when the missing resource is a guardrail, not a model.
func errGuardrailNotFound(id, version string) error {
	if version == "" {
		return awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "guardrail %q not found", id)
	}
	return awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "guardrail %q version %q not found", id, version)
}

// getRevision is get with the KV revision surfaced too, for a CAS write.
func (s *GuardrailStore) getRevision(ctx context.Context, key string) (GuardrailRecord, uint64, bool, error) {
	rec, rev, err := s.store.Get(ctx, key)
	if errors.Is(err, kvstore.ErrNotFound) {
		return GuardrailRecord{}, 0, false, nil
	}
	if err != nil {
		return GuardrailRecord{}, 0, false, err
	}
	return *rec, rev, true, nil
}

// put writes rec under its own account-scoped key.
func (s *GuardrailStore) put(ctx context.Context, rec GuardrailRecord) error {
	return s.store.Set(ctx, guardrailKey(rec.AccountID, rec.GuardrailID), &rec)
}

// update CAS-writes rec back over rev, the revision it was read at, so two
// concurrent mutations of the same guardrail never silently clobber one
// another. A lost race is reported as a retryable ConflictException.
func (s *GuardrailStore) update(ctx context.Context, key string, rec GuardrailRecord, rev uint64) error {
	err := s.store.CompareAndSet(ctx, key, &rec, rev)
	if errors.Is(err, kvstore.ErrConflict) {
		return awserrors.Errorf(awserrors.ErrorConflictException,
			"bedrock: guardrail %s was modified concurrently; retry the request", rec.GuardrailID)
	}
	return err
}

// contentPolicyToSDK translates the stored *Config shape back to the
// AWS-parity *GuardrailContentPolicy GetGuardrail returns. The two shapes are
// field-for-field identical; only the Go type names differ between the
// create/update input and the read-back output.
func contentPolicyToSDK(cfg *bedrock.GuardrailContentPolicyConfig) *bedrock.GuardrailContentPolicy {
	if cfg == nil {
		return nil
	}
	filters := make([]*bedrock.GuardrailContentFilter, 0, len(cfg.FiltersConfig))
	for _, f := range cfg.FiltersConfig {
		if f == nil {
			continue
		}
		filters = append(filters, &bedrock.GuardrailContentFilter{
			InputStrength:  f.InputStrength,
			OutputStrength: f.OutputStrength,
			Type:           f.Type,
		})
	}
	return &bedrock.GuardrailContentPolicy{Filters: filters}
}

// contextualGroundingPolicyToSDK is contentPolicyToSDK's sibling for the
// contextual-grounding policy.
func contextualGroundingPolicyToSDK(cfg *bedrock.GuardrailContextualGroundingPolicyConfig) *bedrock.GuardrailContextualGroundingPolicy {
	if cfg == nil {
		return nil
	}
	filters := make([]*bedrock.GuardrailContextualGroundingFilter, 0, len(cfg.FiltersConfig))
	for _, f := range cfg.FiltersConfig {
		if f == nil {
			continue
		}
		filters = append(filters, &bedrock.GuardrailContextualGroundingFilter{
			Threshold: f.Threshold,
			Type:      f.Type,
		})
	}
	return &bedrock.GuardrailContextualGroundingPolicy{Filters: filters}
}

// sensitiveInformationPolicyToSDK is contentPolicyToSDK's sibling for the PII
// and custom-regex policy.
func sensitiveInformationPolicyToSDK(cfg *bedrock.GuardrailSensitiveInformationPolicyConfig) *bedrock.GuardrailSensitiveInformationPolicy {
	if cfg == nil {
		return nil
	}
	pii := make([]*bedrock.GuardrailPiiEntity, 0, len(cfg.PiiEntitiesConfig))
	for _, e := range cfg.PiiEntitiesConfig {
		if e == nil {
			continue
		}
		pii = append(pii, &bedrock.GuardrailPiiEntity{Action: e.Action, Type: e.Type})
	}
	regexes := make([]*bedrock.GuardrailRegex, 0, len(cfg.RegexesConfig))
	for _, r := range cfg.RegexesConfig {
		if r == nil {
			continue
		}
		regexes = append(regexes, &bedrock.GuardrailRegex{
			Action:      r.Action,
			Description: r.Description,
			Name:        r.Name,
			Pattern:     r.Pattern,
		})
	}
	return &bedrock.GuardrailSensitiveInformationPolicy{PiiEntities: pii, Regexes: regexes}
}

// topicPolicyToSDK is contentPolicyToSDK's sibling for the denied-topics
// policy.
func topicPolicyToSDK(cfg *bedrock.GuardrailTopicPolicyConfig) *bedrock.GuardrailTopicPolicy {
	if cfg == nil {
		return nil
	}
	topics := make([]*bedrock.GuardrailTopic, 0, len(cfg.TopicsConfig))
	for _, t := range cfg.TopicsConfig {
		if t == nil {
			continue
		}
		topics = append(topics, &bedrock.GuardrailTopic{
			Definition: t.Definition,
			Examples:   t.Examples,
			Name:       t.Name,
			Type:       t.Type,
		})
	}
	return &bedrock.GuardrailTopicPolicy{Topics: topics}
}

// wordPolicyToSDK is contentPolicyToSDK's sibling for the word/phrase
// blocklist and managed profanity list.
func wordPolicyToSDK(cfg *bedrock.GuardrailWordPolicyConfig) *bedrock.GuardrailWordPolicy {
	if cfg == nil {
		return nil
	}
	managed := make([]*bedrock.GuardrailManagedWords, 0, len(cfg.ManagedWordListsConfig))
	for _, m := range cfg.ManagedWordListsConfig {
		if m == nil {
			continue
		}
		managed = append(managed, &bedrock.GuardrailManagedWords{Type: m.Type})
	}
	words := make([]*bedrock.GuardrailWord, 0, len(cfg.WordsConfig))
	for _, w := range cfg.WordsConfig {
		if w == nil {
			continue
		}
		words = append(words, &bedrock.GuardrailWord{Text: w.Text})
	}
	return &bedrock.GuardrailWordPolicy{ManagedWordLists: managed, Words: words}
}

// guardrailViewToGetOutput builds a GetGuardrailOutput for either the DRAFT
// (status is the record's live Status) or a numbered snapshot (status is
// always READY: it was only ever taken from a DRAFT that had already
// compiled).
func guardrailViewToGetOutput(arn, id, status, version string, view guardrailView) *bedrock.GetGuardrailOutput {
	return &bedrock.GetGuardrailOutput{
		BlockedInputMessaging:      aws.String(view.BlockedInputMessaging),
		BlockedOutputsMessaging:    aws.String(view.BlockedOutputsMessaging),
		ContentPolicy:              contentPolicyToSDK(view.ContentPolicy),
		ContextualGroundingPolicy:  contextualGroundingPolicyToSDK(view.ContextualGroundingPolicy),
		CreatedAt:                  aws.Time(view.CreatedAt),
		Description:                nonEmptyStringPtr(view.Description),
		GuardrailArn:               aws.String(arn),
		GuardrailId:                aws.String(id),
		Name:                       aws.String(view.Name),
		SensitiveInformationPolicy: sensitiveInformationPolicyToSDK(view.SensitiveInformationPolicy),
		Status:                     aws.String(status),
		TopicPolicy:                topicPolicyToSDK(view.TopicPolicy),
		UpdatedAt:                  aws.Time(view.UpdatedAt),
		Version:                    aws.String(version),
		WordPolicy:                 wordPolicyToSDK(view.WordPolicy),
	}
}

// CreateGuardrail validates the required messaging/name fields, mints a
// GuardrailID + ARN, and writes the DRAFT with Status READY: v1's policies
// (word/PII deterministic, content/topic/contextual-grounding scaffolded) all
// compile synchronously, so there is no Creating->InService async transition
// the way provisioned throughput has.
func CreateGuardrail(ctx context.Context, accountID string, store *GuardrailStore, input *bedrock.CreateGuardrailInput) (*bedrock.CreateGuardrailOutput, error) {
	if input == nil || aws.StringValue(input.Name) == "" ||
		aws.StringValue(input.BlockedInputMessaging) == "" || aws.StringValue(input.BlockedOutputsMessaging) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}

	id := uuid.NewV4().String()
	arn := FormatGuardrailARN(store.region, accountID, id)
	now := time.Now().UTC()

	rec := GuardrailRecord{
		ARN:         arn,
		GuardrailID: id,
		AccountID:   accountID,
		Status:      bedrock.GuardrailStatusReady,
		guardrailView: guardrailView{
			Name:                       aws.StringValue(input.Name),
			Description:                aws.StringValue(input.Description),
			BlockedInputMessaging:      aws.StringValue(input.BlockedInputMessaging),
			BlockedOutputsMessaging:    aws.StringValue(input.BlockedOutputsMessaging),
			WordPolicy:                 input.WordPolicyConfig,
			SensitiveInformationPolicy: input.SensitiveInformationPolicyConfig,
			ContentPolicy:              input.ContentPolicyConfig,
			TopicPolicy:                input.TopicPolicyConfig,
			ContextualGroundingPolicy:  input.ContextualGroundingPolicyConfig,
			CreatedAt:                  now,
			UpdatedAt:                  now,
		},
	}

	if err := store.put(ctx, rec); err != nil {
		return nil, err
	}

	return &bedrock.CreateGuardrailOutput{
		CreatedAt:    aws.Time(now),
		GuardrailArn: aws.String(arn),
		GuardrailId:  aws.String(id),
		Version:      aws.String(guardrailDraftVersion),
	}, nil
}

// GetGuardrail honours input.GuardrailVersion (DRAFT when empty): a numbered
// version returns its frozen snapshot, never the current DRAFT. A foreign
// account or an unknown id/version both report ResourceNotFoundException, so
// a caller cannot distinguish "not yours" from "does not exist".
func GetGuardrail(ctx context.Context, accountID string, store *GuardrailStore, input *bedrock.GetGuardrailInput) (*bedrock.GetGuardrailOutput, error) {
	if input == nil || aws.StringValue(input.GuardrailIdentifier) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id, err := resolveGuardrailID(aws.StringValue(input.GuardrailIdentifier), store.region, accountID)
	if err != nil {
		return nil, err
	}

	rec, found, err := store.get(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errGuardrailNotFound(id, "")
	}

	version := aws.StringValue(input.GuardrailVersion)
	if version == "" || version == guardrailDraftVersion {
		return guardrailViewToGetOutput(rec.ARN, id, rec.Status, guardrailDraftVersion, rec.guardrailView), nil
	}

	snap, ok := rec.Versions[version]
	if !ok {
		return nil, errGuardrailNotFound(id, version)
	}
	return guardrailViewToGetOutput(rec.ARN, id, bedrock.GuardrailStatusReady, snap.Version, snap.guardrailView), nil
}

// ListGuardrails returns the caller account's own guardrails, sorted by
// creation time (id as a deterministic tie-breaker), so one tenant never sees
// another's and repeated calls return a stable order.
func ListGuardrails(ctx context.Context, accountID string, store *GuardrailStore, _ *bedrock.ListGuardrailsInput) (*bedrock.ListGuardrailsOutput, error) {
	recs, err := store.store.List(ctx, accountID+"/")
	if err != nil {
		return nil, err
	}

	slices.SortFunc(recs, func(a, b GuardrailRecord) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.GuardrailID, b.GuardrailID)
	})

	summaries := make([]*bedrock.GuardrailSummary, 0, len(recs))
	for _, rec := range recs {
		summaries = append(summaries, &bedrock.GuardrailSummary{
			Arn:         aws.String(rec.ARN),
			CreatedAt:   aws.Time(rec.CreatedAt),
			Description: nonEmptyStringPtr(rec.Description),
			Id:          aws.String(rec.GuardrailID),
			Name:        aws.String(rec.Name),
			Status:      aws.String(rec.Status),
			UpdatedAt:   aws.Time(rec.UpdatedAt),
			Version:     aws.String(guardrailDraftVersion),
		})
	}
	return &bedrock.ListGuardrailsOutput{Guardrails: summaries}, nil
}

// UpdateGuardrail mutates the DRAFT only. AWS's own UpdateGuardrailInput
// carries no GuardrailVersion field at all — there is structurally no way to
// address a numbered version through this op, which is how AWS enforces
// "only DRAFT mutates" without a runtime check.
func UpdateGuardrail(ctx context.Context, accountID string, store *GuardrailStore, input *bedrock.UpdateGuardrailInput) (*bedrock.UpdateGuardrailOutput, error) {
	if input == nil || aws.StringValue(input.GuardrailIdentifier) == "" || aws.StringValue(input.Name) == "" ||
		aws.StringValue(input.BlockedInputMessaging) == "" || aws.StringValue(input.BlockedOutputsMessaging) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id, err := resolveGuardrailID(aws.StringValue(input.GuardrailIdentifier), store.region, accountID)
	if err != nil {
		return nil, err
	}

	key := guardrailKey(accountID, id)
	rec, rev, found, err := store.getRevision(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errGuardrailNotFound(id, "")
	}

	rec.Name = aws.StringValue(input.Name)
	rec.Description = aws.StringValue(input.Description)
	rec.BlockedInputMessaging = aws.StringValue(input.BlockedInputMessaging)
	rec.BlockedOutputsMessaging = aws.StringValue(input.BlockedOutputsMessaging)
	rec.WordPolicy = input.WordPolicyConfig
	rec.SensitiveInformationPolicy = input.SensitiveInformationPolicyConfig
	rec.ContentPolicy = input.ContentPolicyConfig
	rec.TopicPolicy = input.TopicPolicyConfig
	rec.ContextualGroundingPolicy = input.ContextualGroundingPolicyConfig
	rec.UpdatedAt = time.Now().UTC()

	if err := store.update(ctx, key, rec, rev); err != nil {
		return nil, err
	}

	return &bedrock.UpdateGuardrailOutput{
		GuardrailArn: aws.String(rec.ARN),
		GuardrailId:  aws.String(id),
		UpdatedAt:    aws.Time(rec.UpdatedAt),
		Version:      aws.String(guardrailDraftVersion),
	}, nil
}

// DeleteGuardrail is version-aware: a specific numbered GuardrailVersion
// deletes only that snapshot; an absent version deletes the whole guardrail
// (DRAFT and every snapshot). Both directions are idempotent: deleting an
// already-absent guardrail or an already-absent version is a no-op success.
func DeleteGuardrail(ctx context.Context, accountID string, store *GuardrailStore, input *bedrock.DeleteGuardrailInput) (*bedrock.DeleteGuardrailOutput, error) {
	if input == nil || aws.StringValue(input.GuardrailIdentifier) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id, err := resolveGuardrailID(aws.StringValue(input.GuardrailIdentifier), store.region, accountID)
	if err != nil {
		return nil, err
	}

	key := guardrailKey(accountID, id)
	version := aws.StringValue(input.GuardrailVersion)

	if version == "" {
		if err := store.store.Delete(ctx, key); err != nil {
			return nil, err
		}
		return &bedrock.DeleteGuardrailOutput{}, nil
	}

	rec, rev, found, err := store.getRevision(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return &bedrock.DeleteGuardrailOutput{}, nil
	}
	if version == guardrailDraftVersion {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException,
			"bedrock: guardrail %s DRAFT cannot be deleted by version; omit guardrailVersion to delete the whole guardrail", id)
	}

	delete(rec.Versions, version)
	if err := store.update(ctx, key, rec, rev); err != nil {
		return nil, err
	}
	return &bedrock.DeleteGuardrailOutput{}, nil
}

// CreateGuardrailVersion snapshots the current DRAFT into the next
// immutable numbered version and returns that version number. NextVersion is
// a monotonic counter on the record itself, not len(Versions), so a version
// deleted later never gets its number reused.
func CreateGuardrailVersion(ctx context.Context, accountID string, store *GuardrailStore, input *bedrock.CreateGuardrailVersionInput) (*bedrock.CreateGuardrailVersionOutput, error) {
	if input == nil || aws.StringValue(input.GuardrailIdentifier) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id, err := resolveGuardrailID(aws.StringValue(input.GuardrailIdentifier), store.region, accountID)
	if err != nil {
		return nil, err
	}

	key := guardrailKey(accountID, id)
	rec, rev, found, err := store.getRevision(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errGuardrailNotFound(id, "")
	}

	rec.NextVersion++
	version := strconv.Itoa(rec.NextVersion)
	if rec.Versions == nil {
		rec.Versions = map[string]GuardrailVersionSnapshot{}
	}
	rec.Versions[version] = GuardrailVersionSnapshot{Version: version, guardrailView: rec.guardrailView}

	if err := store.update(ctx, key, rec, rev); err != nil {
		return nil, err
	}

	return &bedrock.CreateGuardrailVersionOutput{GuardrailId: aws.String(id), Version: aws.String(version)}, nil
}

// ApplyGuardrail runs the guardrail engine over input.Content for
// input.Source, honouring input.GuardrailVersion (DRAFT or a numbered
// snapshot), and returns the aws-sdk-go bedrockruntime shape the runtime op
// hands back to the caller. Unknown/foreign guardrail or version both report
// ResourceNotFoundException, the same as the control-plane ops. embedder
// drives topicPolicy's semantic match; a nil embedder falls back to the
// literal matcher (see assessTopicPolicy).
func ApplyGuardrail(ctx context.Context, accountID string, store *GuardrailStore, embedder Embedder, input *bedrockruntime.ApplyGuardrailInput) (*bedrockruntime.ApplyGuardrailOutput, error) {
	if input == nil || aws.StringValue(input.GuardrailIdentifier) == "" ||
		aws.StringValue(input.GuardrailVersion) == "" || aws.StringValue(input.Source) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id, err := resolveGuardrailID(aws.StringValue(input.GuardrailIdentifier), store.region, accountID)
	if err != nil {
		return nil, err
	}

	rec, found, err := store.get(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errGuardrailNotFound(id, "")
	}

	view := rec.guardrailView
	version := aws.StringValue(input.GuardrailVersion)
	if version != guardrailDraftVersion {
		snap, ok := rec.Versions[version]
		if !ok {
			return nil, errGuardrailNotFound(id, version)
		}
		view = snap.guardrailView
	}

	texts := make([]string, 0, len(input.Content))
	for _, c := range input.Content {
		if c == nil || c.Text == nil {
			continue
		}
		texts = append(texts, aws.StringValue(c.Text.Text))
	}

	action, assessments, outputTexts, usage := applyGuardrailPolicies(ctx, embedder, view, texts, aws.StringValue(input.Source))

	outputs := make([]*bedrockruntime.GuardrailOutputContent, 0, len(outputTexts))
	if action == bedrockruntime.GuardrailActionGuardrailIntervened {
		message := view.BlockedInputMessaging
		if aws.StringValue(input.Source) == bedrockruntime.GuardrailContentSourceOutput {
			message = view.BlockedOutputsMessaging
		}
		outputs = append(outputs, &bedrockruntime.GuardrailOutputContent{Text: aws.String(message)})
	} else {
		for _, t := range outputTexts {
			outputs = append(outputs, &bedrockruntime.GuardrailOutputContent{Text: aws.String(t)})
		}
	}

	return &bedrockruntime.ApplyGuardrailOutput{
		Action:      aws.String(action),
		Assessments: assessments,
		Outputs:     outputs,
		Usage:       usage,
	}, nil
}
