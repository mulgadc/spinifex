package gateway_bedrock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	grCallerAccount = "000000000001"
	grOtherCaller   = "000000000002"
)

func newGuardrailTestStore(t *testing.T) *GuardrailStore {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return NewGuardrailStore(js, 1, ptTestRegion)
}

// createGuardrailInput builds a minimal valid CreateGuardrailInput carrying
// one policy of each of the five kinds, so tests can prove the full config
// round-trips through Create/Get untouched.
func createGuardrailInput(name string) *bedrock.CreateGuardrailInput {
	return &bedrock.CreateGuardrailInput{
		Name:                    aws.String(name),
		Description:             aws.String("blocks bad words and redacts email"),
		BlockedInputMessaging:   aws.String("Your input violates our policy."),
		BlockedOutputsMessaging: aws.String("The model response violates our policy."),
		WordPolicyConfig: &bedrock.GuardrailWordPolicyConfig{
			WordsConfig: []*bedrock.GuardrailWordConfig{
				{Text: aws.String("badword")},
			},
			ManagedWordListsConfig: []*bedrock.GuardrailManagedWordsConfig{
				{Type: aws.String(bedrock.GuardrailManagedWordsTypeProfanity)},
			},
		},
		SensitiveInformationPolicyConfig: &bedrock.GuardrailSensitiveInformationPolicyConfig{
			PiiEntitiesConfig: []*bedrock.GuardrailPiiEntityConfig{
				{Type: aws.String(bedrock.GuardrailPiiEntityTypeEmail), Action: aws.String(bedrock.GuardrailSensitiveInformationActionAnonymize)},
			},
			RegexesConfig: []*bedrock.GuardrailRegexConfig{
				{Name: aws.String("account-id"), Pattern: aws.String(`\d{12}`), Action: aws.String(bedrock.GuardrailSensitiveInformationActionBlock)},
			},
		},
		ContentPolicyConfig: &bedrock.GuardrailContentPolicyConfig{
			FiltersConfig: []*bedrock.GuardrailContentFilterConfig{
				{Type: aws.String(bedrock.GuardrailContentFilterTypeHate), InputStrength: aws.String(bedrock.GuardrailFilterStrengthHigh), OutputStrength: aws.String(bedrock.GuardrailFilterStrengthHigh)},
			},
		},
		TopicPolicyConfig: &bedrock.GuardrailTopicPolicyConfig{
			TopicsConfig: []*bedrock.GuardrailTopicConfig{
				{Name: aws.String("competitors"), Definition: aws.String("mentions of competitor products"), Type: aws.String(bedrock.GuardrailTopicTypeDeny)},
			},
		},
		ContextualGroundingPolicyConfig: &bedrock.GuardrailContextualGroundingPolicyConfig{
			FiltersConfig: []*bedrock.GuardrailContextualGroundingFilterConfig{
				{Type: aws.String(bedrock.GuardrailContextualGroundingFilterTypeGrounding), Threshold: aws.Float64(0.5)},
			},
		},
	}
}

// TestCreateGuardrail_MintsARNAndDraft covers Create's happy path: a
// well-formed, self-parseable ARN, Status READY, and every policy config
// persisted verbatim (round-tripped back through Get).
func TestCreateGuardrail_MintsARNAndDraft(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	out, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("my-guardrail"))
	require.NoError(t, err)
	require.NotNil(t, out.GuardrailArn)
	require.NotNil(t, out.GuardrailId)
	assert.Equal(t, guardrailDraftVersion, aws.StringValue(out.Version))

	parsed, err := ParseGuardrailARN(aws.StringValue(out.GuardrailArn), ptTestRegion, grCallerAccount)
	require.NoError(t, err, "Create must return a well-formed, self-parseable ARN")
	assert.Equal(t, aws.StringValue(out.GuardrailId), parsed.ID)

	getOut, err := GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: out.GuardrailId})
	require.NoError(t, err)
	assert.Equal(t, bedrock.GuardrailStatusReady, aws.StringValue(getOut.Status))
	assert.Equal(t, "my-guardrail", aws.StringValue(getOut.Name))
	assert.Equal(t, guardrailDraftVersion, aws.StringValue(getOut.Version))

	require.NotNil(t, getOut.WordPolicy)
	require.Len(t, getOut.WordPolicy.Words, 1)
	assert.Equal(t, "badword", aws.StringValue(getOut.WordPolicy.Words[0].Text))
	require.Len(t, getOut.WordPolicy.ManagedWordLists, 1)

	require.NotNil(t, getOut.SensitiveInformationPolicy)
	require.Len(t, getOut.SensitiveInformationPolicy.PiiEntities, 1)
	assert.Equal(t, bedrock.GuardrailPiiEntityTypeEmail, aws.StringValue(getOut.SensitiveInformationPolicy.PiiEntities[0].Type))
	require.Len(t, getOut.SensitiveInformationPolicy.Regexes, 1)
	assert.Equal(t, `\d{12}`, aws.StringValue(getOut.SensitiveInformationPolicy.Regexes[0].Pattern))

	require.NotNil(t, getOut.ContentPolicy)
	require.Len(t, getOut.ContentPolicy.Filters, 1)
	assert.Equal(t, bedrock.GuardrailContentFilterTypeHate, aws.StringValue(getOut.ContentPolicy.Filters[0].Type))

	require.NotNil(t, getOut.TopicPolicy)
	require.Len(t, getOut.TopicPolicy.Topics, 1)
	assert.Equal(t, "competitors", aws.StringValue(getOut.TopicPolicy.Topics[0].Name))

	require.NotNil(t, getOut.ContextualGroundingPolicy)
	require.Len(t, getOut.ContextualGroundingPolicy.Filters, 1)
	assert.InDelta(t, 0.5, aws.Float64Value(getOut.ContextualGroundingPolicy.Filters[0].Threshold), 0.0001)
}

// TestCreateGuardrail_RejectsMissingRequiredFields covers the required-field
// validation AWS's own CreateGuardrailInput.Validate() enforces.
func TestCreateGuardrail_RejectsMissingRequiredFields(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	cases := []*bedrock.CreateGuardrailInput{
		nil,
		{BlockedInputMessaging: aws.String("x"), BlockedOutputsMessaging: aws.String("y")},
		{Name: aws.String("x"), BlockedOutputsMessaging: aws.String("y")},
		{Name: aws.String("x"), BlockedInputMessaging: aws.String("y")},
	}
	for _, input := range cases {
		_, err := CreateGuardrail(ctx, grCallerAccount, store, input)
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorValidationException, awserrors.ValidErrorCodeFromError(err))
	}
}

// TestGetGuardrail_NotFound covers both a bare id that was never created and
// a foreign account presenting its own accountID: both must read as
// not-found, never leaking whether the id exists elsewhere.
func TestGetGuardrail_NotFound(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	out, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("my-guardrail"))
	require.NoError(t, err)

	_, err = GetGuardrail(ctx, grOtherCaller, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: out.GuardrailId})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())

	_, err = GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: aws.String("does-not-exist")})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestCreateGuardrailVersion_SnapshotsImmutably is the plan's core version
// invariant: a numbered version freezes the DRAFT at creation time, and a
// later DRAFT mutation never reaches an already-created snapshot.
func TestCreateGuardrailVersion_SnapshotsImmutably(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("v1-name"))
	require.NoError(t, err)

	verOut, err := CreateGuardrailVersion(ctx, grCallerAccount, store, &bedrock.CreateGuardrailVersionInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)
	assert.Equal(t, "1", aws.StringValue(verOut.Version))

	// Mutate the DRAFT after the snapshot was taken.
	_, err = UpdateGuardrail(ctx, grCallerAccount, store, &bedrock.UpdateGuardrailInput{
		GuardrailIdentifier:     createOut.GuardrailId,
		Name:                    aws.String("v2-name"),
		BlockedInputMessaging:   aws.String("updated input message"),
		BlockedOutputsMessaging: aws.String("updated output message"),
	})
	require.NoError(t, err)

	// DRAFT reflects the mutation.
	draftOut, err := GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)
	assert.Equal(t, "v2-name", aws.StringValue(draftOut.Name))

	// The numbered snapshot still reads the original name, untouched by the
	// later DRAFT mutation.
	snapOut, err := GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{
		GuardrailIdentifier: createOut.GuardrailId,
		GuardrailVersion:    aws.String("1"),
	})
	require.NoError(t, err)
	assert.Equal(t, "v1-name", aws.StringValue(snapOut.Name))
	assert.Equal(t, "1", aws.StringValue(snapOut.Version))
	assert.Equal(t, bedrock.GuardrailStatusReady, aws.StringValue(snapOut.Status))

	// A second version bumps the counter monotonically.
	verOut2, err := CreateGuardrailVersion(ctx, grCallerAccount, store, &bedrock.CreateGuardrailVersionInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)
	assert.Equal(t, "2", aws.StringValue(verOut2.Version))
}

// TestGetGuardrail_VersionField covers Get's version handling: empty and
// "DRAFT" both mean the mutable working copy; a numbered version that was
// never created is not-found.
func TestGetGuardrail_VersionField(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("my-guardrail"))
	require.NoError(t, err)

	outEmpty, err := GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)
	assert.Equal(t, guardrailDraftVersion, aws.StringValue(outEmpty.Version))

	outDraft, err := GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{
		GuardrailIdentifier: createOut.GuardrailId,
		GuardrailVersion:    aws.String(guardrailDraftVersion),
	})
	require.NoError(t, err)
	assert.Equal(t, guardrailDraftVersion, aws.StringValue(outDraft.Version))

	_, err = GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{
		GuardrailIdentifier: createOut.GuardrailId,
		GuardrailVersion:    aws.String("1"),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestListGuardrails_AccountScoped is the plan's explicit isolation
// requirement: account B never sees account A's guardrail.
func TestListGuardrails_AccountScoped(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	_, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("acct-a-guardrail"))
	require.NoError(t, err)

	listA, err := ListGuardrails(ctx, grCallerAccount, store, nil)
	require.NoError(t, err)
	require.Len(t, listA.Guardrails, 1)
	assert.Equal(t, "acct-a-guardrail", aws.StringValue(listA.Guardrails[0].Name))

	listB, err := ListGuardrails(ctx, grOtherCaller, store, nil)
	require.NoError(t, err)
	assert.Empty(t, listB.Guardrails, "account B must not see account A's guardrail")
}

// TestListGuardrails_SortedDeterministically covers List's ordering
// guarantee: repeated calls return guardrails in the same (creation-time,
// then id) order.
func TestListGuardrails_SortedDeterministically(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	_, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("first"))
	require.NoError(t, err)
	_, err = CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("second"))
	require.NoError(t, err)

	list1, err := ListGuardrails(ctx, grCallerAccount, store, nil)
	require.NoError(t, err)
	list2, err := ListGuardrails(ctx, grCallerAccount, store, nil)
	require.NoError(t, err)
	require.Len(t, list1.Guardrails, 2)
	require.Len(t, list2.Guardrails, 2)
	assert.Equal(t, aws.StringValue(list1.Guardrails[0].Id), aws.StringValue(list2.Guardrails[0].Id))
	assert.Equal(t, aws.StringValue(list1.Guardrails[1].Id), aws.StringValue(list2.Guardrails[1].Id))
}

// TestUpdateGuardrail_MutatesDraftOnly covers Update's scope: it changes the
// DRAFT's fields (including swapping a policy config wholesale) and never
// touches the Versions map.
func TestUpdateGuardrail_MutatesDraftOnly(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("original"))
	require.NoError(t, err)
	_, err = CreateGuardrailVersion(ctx, grCallerAccount, store, &bedrock.CreateGuardrailVersionInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)

	updateInput := createGuardrailInput("updated")
	_, err = UpdateGuardrail(ctx, grCallerAccount, store, &bedrock.UpdateGuardrailInput{
		GuardrailIdentifier:              createOut.GuardrailId,
		Name:                             updateInput.Name,
		BlockedInputMessaging:            updateInput.BlockedInputMessaging,
		BlockedOutputsMessaging:          updateInput.BlockedOutputsMessaging,
		WordPolicyConfig:                 &bedrock.GuardrailWordPolicyConfig{WordsConfig: []*bedrock.GuardrailWordConfig{{Text: aws.String("newword")}}},
		SensitiveInformationPolicyConfig: updateInput.SensitiveInformationPolicyConfig,
	})
	require.NoError(t, err)

	getOut, err := GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)
	assert.Equal(t, "updated", aws.StringValue(getOut.Name))
	require.Len(t, getOut.WordPolicy.Words, 1)
	assert.Equal(t, "newword", aws.StringValue(getOut.WordPolicy.Words[0].Text))

	// The already-created version "1" still reads the pre-update DRAFT.
	snapOut, err := GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{
		GuardrailIdentifier: createOut.GuardrailId,
		GuardrailVersion:    aws.String("1"),
	})
	require.NoError(t, err)
	assert.Equal(t, "original", aws.StringValue(snapOut.Name))
}

// TestUpdateGuardrail_NotFound guards that Update on a never-created (or
// foreign-account) guardrail reports not-found rather than silently creating
// one.
func TestUpdateGuardrail_NotFound(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	_, err := UpdateGuardrail(ctx, grCallerAccount, store, &bedrock.UpdateGuardrailInput{
		GuardrailIdentifier:     aws.String("does-not-exist"),
		Name:                    aws.String("x"),
		BlockedInputMessaging:   aws.String("y"),
		BlockedOutputsMessaging: aws.String("z"),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestDeleteGuardrail_VersionDeletesOnlyThatSnapshot covers Delete's
// version-aware half: deleting version "1" removes only that snapshot, and
// the DRAFT plus any other version survive.
func TestDeleteGuardrail_VersionDeletesOnlyThatSnapshot(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("my-guardrail"))
	require.NoError(t, err)
	_, err = CreateGuardrailVersion(ctx, grCallerAccount, store, &bedrock.CreateGuardrailVersionInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)
	_, err = CreateGuardrailVersion(ctx, grCallerAccount, store, &bedrock.CreateGuardrailVersionInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)

	_, err = DeleteGuardrail(ctx, grCallerAccount, store, &bedrock.DeleteGuardrailInput{
		GuardrailIdentifier: createOut.GuardrailId,
		GuardrailVersion:    aws.String("1"),
	})
	require.NoError(t, err)

	_, err = GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{
		GuardrailIdentifier: createOut.GuardrailId,
		GuardrailVersion:    aws.String("1"),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())

	// Version "2" and the DRAFT both survive.
	_, err = GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{
		GuardrailIdentifier: createOut.GuardrailId,
		GuardrailVersion:    aws.String("2"),
	})
	require.NoError(t, err)
	_, err = GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)
}

// TestDeleteGuardrail_AbsentVersionIsNoop covers deleting a numbered version
// that was never created: idempotent-friendly, not an error.
func TestDeleteGuardrail_AbsentVersionIsNoop(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("my-guardrail"))
	require.NoError(t, err)

	_, err = DeleteGuardrail(ctx, grCallerAccount, store, &bedrock.DeleteGuardrailInput{
		GuardrailIdentifier: createOut.GuardrailId,
		GuardrailVersion:    aws.String("99"),
	})
	require.NoError(t, err)

	_, err = GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err, "the DRAFT must survive deleting a version that never existed")
}

// TestDeleteGuardrail_RejectsDeletingDraftByVersion pins the invariant that
// "DRAFT" is not a deletable snapshot: it must be deleted by omitting
// GuardrailVersion entirely (which deletes the whole guardrail).
func TestDeleteGuardrail_RejectsDeletingDraftByVersion(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("my-guardrail"))
	require.NoError(t, err)

	_, err = DeleteGuardrail(ctx, grCallerAccount, store, &bedrock.DeleteGuardrailInput{
		GuardrailIdentifier: createOut.GuardrailId,
		GuardrailVersion:    aws.String(guardrailDraftVersion),
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, awserrors.ValidErrorCodeFromError(err))
}

// TestDeleteGuardrail_WholeGuardrailAndAbsentIsNoop covers Delete's
// whole-guardrail half (no GuardrailVersion) and its idempotence on an
// already-absent guardrail.
func TestDeleteGuardrail_WholeGuardrailAndAbsentIsNoop(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("my-guardrail"))
	require.NoError(t, err)
	_, err = CreateGuardrailVersion(ctx, grCallerAccount, store, &bedrock.CreateGuardrailVersionInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)

	_, err = DeleteGuardrail(ctx, grCallerAccount, store, &bedrock.DeleteGuardrailInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)

	_, err = GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: createOut.GuardrailId})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())

	// Deleting the already-absent guardrail again is a no-op success.
	_, err = DeleteGuardrail(ctx, grCallerAccount, store, &bedrock.DeleteGuardrailInput{GuardrailIdentifier: aws.String("never-created")})
	require.NoError(t, err)
}

// TestCreateGuardrailVersion_NotFound guards that versioning a never-created
// (or foreign-account) guardrail reports not-found.
func TestCreateGuardrailVersion_NotFound(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	_, err := CreateGuardrailVersion(ctx, grCallerAccount, store, &bedrock.CreateGuardrailVersionInput{GuardrailIdentifier: aws.String("does-not-exist")})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestGuardrailID_ResolvesBareOrARN covers the shape every guardrail op's
// GuardrailIdentifier field allows: a bare id or a full ARN, resolved through
// Get to prove both reach the same record.
func TestGuardrailID_ResolvesBareOrARN(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("my-guardrail"))
	require.NoError(t, err)

	byID, err := GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)
	byARN, err := GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: createOut.GuardrailArn})
	require.NoError(t, err)
	assert.Equal(t, aws.StringValue(byID.GuardrailId), aws.StringValue(byARN.GuardrailId))

	// A foreign-account ARN never resolves under the caller's own account.
	foreignARN := FormatGuardrailARN(ptTestRegion, grOtherCaller, aws.StringValue(createOut.GuardrailId))
	_, err = GetGuardrail(ctx, grCallerAccount, store, &bedrock.GetGuardrailInput{GuardrailIdentifier: aws.String(foreignARN)})
	require.Error(t, err)
}
