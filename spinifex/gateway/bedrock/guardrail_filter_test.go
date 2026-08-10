package gateway_bedrock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyGuardrailPolicies_WordAndPII is the filter-engine table test: pure
// guardrailView + texts in, (action, assessments, outputs) out, no I/O.
func TestApplyGuardrailPolicies_WordAndPII(t *testing.T) {
	blockWord := guardrailView{
		WordPolicy: &bedrock.GuardrailWordPolicyConfig{
			WordsConfig: []*bedrock.GuardrailWordConfig{{Text: aws.String("badword")}},
		},
	}
	managedProfanity := guardrailView{
		WordPolicy: &bedrock.GuardrailWordPolicyConfig{
			ManagedWordListsConfig: []*bedrock.GuardrailManagedWordsConfig{
				{Type: aws.String(bedrock.GuardrailManagedWordsTypeProfanity)},
			},
		},
	}
	blockPII := func(entityType string) guardrailView {
		return guardrailView{
			SensitiveInformationPolicy: &bedrock.GuardrailSensitiveInformationPolicyConfig{
				PiiEntitiesConfig: []*bedrock.GuardrailPiiEntityConfig{
					{Type: aws.String(entityType), Action: aws.String(bedrock.GuardrailSensitiveInformationActionBlock)},
				},
			},
		}
	}
	anonymizePII := func(entityType string) guardrailView {
		return guardrailView{
			SensitiveInformationPolicy: &bedrock.GuardrailSensitiveInformationPolicyConfig{
				PiiEntitiesConfig: []*bedrock.GuardrailPiiEntityConfig{
					{Type: aws.String(entityType), Action: aws.String(bedrock.GuardrailSensitiveInformationActionAnonymize)},
				},
			},
		}
	}

	cases := []struct {
		name         string
		view         guardrailView
		texts        []string
		source       string
		wantAction   string
		wantOutput   string
		wantBlocked  bool
		wantAnonym   bool
		wantEntities []string
	}{
		{
			name:       "clean text is NONE",
			view:       blockWord,
			texts:      []string{"nothing to see here"},
			source:     bedrockruntime.GuardrailContentSourceInput,
			wantAction: bedrockruntime.GuardrailActionNone,
			wantOutput: "nothing to see here",
		},
		{
			name:        "custom blocklist word intervenes",
			view:        blockWord,
			texts:       []string{"this contains a badword right here"},
			source:      bedrockruntime.GuardrailContentSourceInput,
			wantAction:  bedrockruntime.GuardrailActionGuardrailIntervened,
			wantBlocked: true,
		},
		{
			name:        "managed profanity list intervenes",
			view:        managedProfanity,
			texts:       []string{"that is such bullshit honestly"},
			source:      bedrockruntime.GuardrailContentSourceOutput,
			wantAction:  bedrockruntime.GuardrailActionGuardrailIntervened,
			wantBlocked: true,
		},
		{
			name:         "email detected and anonymized",
			view:         anonymizePII(bedrock.GuardrailPiiEntityTypeEmail),
			texts:        []string{"reach me at jane@example.com please"},
			source:       bedrockruntime.GuardrailContentSourceInput,
			wantAction:   bedrockruntime.GuardrailActionNone,
			wantOutput:   "reach me at {EMAIL} please",
			wantAnonym:   true,
			wantEntities: []string{bedrock.GuardrailPiiEntityTypeEmail},
		},
		{
			name:         "phone number detected and anonymized",
			view:         anonymizePII(bedrock.GuardrailPiiEntityTypePhone),
			texts:        []string{"call me at 415-555-0100 tomorrow"},
			source:       bedrockruntime.GuardrailContentSourceInput,
			wantAction:   bedrockruntime.GuardrailActionNone,
			wantOutput:   "call me at {PHONE} tomorrow",
			wantAnonym:   true,
			wantEntities: []string{bedrock.GuardrailPiiEntityTypePhone},
		},
		{
			name:         "SSN detected and blocked",
			view:         blockPII(bedrock.GuardrailPiiEntityTypeUsSocialSecurityNumber),
			texts:        []string{"my ssn is 123-45-6789 ok"},
			source:       bedrockruntime.GuardrailContentSourceInput,
			wantAction:   bedrockruntime.GuardrailActionGuardrailIntervened,
			wantBlocked:  true,
			wantEntities: []string{bedrock.GuardrailPiiEntityTypeUsSocialSecurityNumber},
		},
		{
			name:         "credit card number detected and anonymized",
			view:         anonymizePII(bedrock.GuardrailPiiEntityTypeCreditDebitCardNumber),
			texts:        []string{"card 4111 1111 1111 1111 on file"},
			source:       bedrockruntime.GuardrailContentSourceInput,
			wantAction:   bedrockruntime.GuardrailActionNone,
			wantOutput:   "card {CREDIT_DEBIT_CARD_NUMBER} on file",
			wantAnonym:   true,
			wantEntities: []string{bedrock.GuardrailPiiEntityTypeCreditDebitCardNumber},
		},
		{
			name:         "IPv4 address detected and anonymized",
			view:         anonymizePII(bedrock.GuardrailPiiEntityTypeIpAddress),
			texts:        []string{"connect to 192.168.1.10 now"},
			source:       bedrockruntime.GuardrailContentSourceOutput,
			wantAction:   bedrockruntime.GuardrailActionNone,
			wantOutput:   "connect to {IP_ADDRESS} now",
			wantAnonym:   true,
			wantEntities: []string{bedrock.GuardrailPiiEntityTypeIpAddress},
		},
		{
			name:         "URL detected and anonymized",
			view:         anonymizePII(bedrock.GuardrailPiiEntityTypeUrl),
			texts:        []string{"see https://example.com/path for details"},
			source:       bedrockruntime.GuardrailContentSourceOutput,
			wantAction:   bedrockruntime.GuardrailActionNone,
			wantOutput:   "see {URL} for details",
			wantAnonym:   true,
			wantEntities: []string{bedrock.GuardrailPiiEntityTypeUrl},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, assessments, outputs, usage := applyGuardrailPolicies(tc.view, tc.texts, tc.source)
			assert.Equal(t, tc.wantAction, action)
			require.Len(t, assessments, 1)
			require.NotNil(t, usage)

			if tc.wantOutput != "" {
				require.Len(t, outputs, 1)
				assert.Equal(t, tc.wantOutput, outputs[0])
			}

			if tc.wantBlocked {
				blockedSomewhere := false
				if wp := assessments[0].WordPolicy; wp != nil {
					blockedSomewhere = blockedSomewhere || len(wp.CustomWords) > 0 || len(wp.ManagedWordLists) > 0
				}
				if sip := assessments[0].SensitiveInformationPolicy; sip != nil {
					for _, e := range sip.PiiEntities {
						if aws.StringValue(e.Action) == bedrockruntime.GuardrailSensitiveInformationPolicyActionBlocked {
							blockedSomewhere = true
						}
					}
				}
				assert.True(t, blockedSomewhere, "expected a blocked entry in the assessment")
			}

			if tc.wantAnonym {
				require.NotNil(t, assessments[0].SensitiveInformationPolicy)
				var gotEntities []string
				for _, e := range assessments[0].SensitiveInformationPolicy.PiiEntities {
					assert.Equal(t, bedrockruntime.GuardrailSensitiveInformationPolicyActionAnonymized, aws.StringValue(e.Action))
					gotEntities = append(gotEntities, aws.StringValue(e.Type))
				}
				assert.Equal(t, tc.wantEntities, gotEntities)
			}
		})
	}
}

// TestApplyGuardrailPolicies_CustomRegex covers a guardrail-defined regex
// pattern, independent of the built-in PII entity table.
func TestApplyGuardrailPolicies_CustomRegex(t *testing.T) {
	view := guardrailView{
		SensitiveInformationPolicy: &bedrock.GuardrailSensitiveInformationPolicyConfig{
			RegexesConfig: []*bedrock.GuardrailRegexConfig{
				{Name: aws.String("account-id"), Pattern: aws.String(`\d{12}`), Action: aws.String(bedrock.GuardrailSensitiveInformationActionAnonymize)},
			},
		},
	}

	action, assessments, outputs, _ := applyGuardrailPolicies(view, []string{"account 123456789012 is active"}, bedrockruntime.GuardrailContentSourceInput)
	assert.Equal(t, bedrockruntime.GuardrailActionNone, action)
	require.Len(t, outputs, 1)
	assert.Equal(t, "account {account-id} is active", outputs[0])
	require.NotNil(t, assessments[0].SensitiveInformationPolicy)
	require.Len(t, assessments[0].SensitiveInformationPolicy.Regexes, 1)
	assert.Equal(t, "123456789012", aws.StringValue(assessments[0].SensitiveInformationPolicy.Regexes[0].Match))

	viewBlock := guardrailView{
		SensitiveInformationPolicy: &bedrock.GuardrailSensitiveInformationPolicyConfig{
			RegexesConfig: []*bedrock.GuardrailRegexConfig{
				{Name: aws.String("account-id"), Pattern: aws.String(`\d{12}`), Action: aws.String(bedrock.GuardrailSensitiveInformationActionBlock)},
			},
		},
	}
	action, _, _, _ = applyGuardrailPolicies(viewBlock, []string{"account 123456789012 is active"}, bedrockruntime.GuardrailContentSourceInput)
	assert.Equal(t, bedrockruntime.GuardrailActionGuardrailIntervened, action)
}

// TestApplyGuardrailPolicies_ScaffoldPolicies covers the classifier-backed
// policies: configuring content/topic/contextualGrounding records an
// assessment (so the caller can see the policy was evaluated) but never
// intervenes, since v1 has no classifier to score them.
func TestApplyGuardrailPolicies_ScaffoldPolicies(t *testing.T) {
	view := guardrailView{
		ContentPolicy: &bedrock.GuardrailContentPolicyConfig{
			FiltersConfig: []*bedrock.GuardrailContentFilterConfig{
				{Type: aws.String(bedrock.GuardrailContentFilterTypeHate), InputStrength: aws.String(bedrock.GuardrailFilterStrengthHigh), OutputStrength: aws.String(bedrock.GuardrailFilterStrengthHigh)},
			},
		},
		TopicPolicy: &bedrock.GuardrailTopicPolicyConfig{
			TopicsConfig: []*bedrock.GuardrailTopicConfig{
				{Name: aws.String("competitors"), Definition: aws.String("mentions of competitors"), Type: aws.String(bedrock.GuardrailTopicTypeDeny)},
			},
		},
		ContextualGroundingPolicy: &bedrock.GuardrailContextualGroundingPolicyConfig{
			FiltersConfig: []*bedrock.GuardrailContextualGroundingFilterConfig{
				{Type: aws.String(bedrock.GuardrailContextualGroundingFilterTypeGrounding), Threshold: aws.Float64(0.5)},
			},
		},
	}

	action, assessments, outputs, _ := applyGuardrailPolicies(view, []string{"our competitor's product is inferior"}, bedrockruntime.GuardrailContentSourceInput)
	assert.Equal(t, bedrockruntime.GuardrailActionNone, action)
	assert.Equal(t, []string{"our competitor's product is inferior"}, outputs)

	require.NotNil(t, assessments[0].ContentPolicy)
	assert.Empty(t, assessments[0].ContentPolicy.Filters)
	require.NotNil(t, assessments[0].TopicPolicy)
	assert.Empty(t, assessments[0].TopicPolicy.Topics)
	require.NotNil(t, assessments[0].ContextualGroundingPolicy)
	assert.Empty(t, assessments[0].ContextualGroundingPolicy.Filters)
}

// TestApplyGuardrailPolicies_UnconfiguredPoliciesAreOmitted covers that a
// policy the guardrail never configured has no assessment entry at all,
// rather than an empty scaffold one.
func TestApplyGuardrailPolicies_UnconfiguredPoliciesAreOmitted(t *testing.T) {
	_, assessments, _, _ := applyGuardrailPolicies(guardrailView{}, []string{"hello"}, bedrockruntime.GuardrailContentSourceInput)
	require.Len(t, assessments, 1)
	assert.Nil(t, assessments[0].WordPolicy)
	assert.Nil(t, assessments[0].SensitiveInformationPolicy)
	assert.Nil(t, assessments[0].ContentPolicy)
	assert.Nil(t, assessments[0].TopicPolicy)
	assert.Nil(t, assessments[0].ContextualGroundingPolicy)
}

// applyGuardrailInput is a small ApplyGuardrail request builder for the
// contract tests below, mirroring createGuardrailInput's role for CRUD.
func applyGuardrailInput(guardrailID *string, version, source, text string) *bedrockruntime.ApplyGuardrailInput {
	return &bedrockruntime.ApplyGuardrailInput{
		GuardrailIdentifier: guardrailID,
		GuardrailVersion:    aws.String(version),
		Source:              aws.String(source),
		Content: []*bedrockruntime.GuardrailContentBlock{
			{Text: &bedrockruntime.GuardrailTextBlock{Text: aws.String(text)}},
		},
	}
}

// TestApplyGuardrail_InputBlock covers the runtime op end to end: a
// word-policy hit on INPUT content intervenes.
func TestApplyGuardrail_InputBlock(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("apply-guardrail-block"))
	require.NoError(t, err)

	out, err := ApplyGuardrail(ctx, grCallerAccount, store, applyGuardrailInput(createOut.GuardrailId, guardrailDraftVersion, bedrockruntime.GuardrailContentSourceInput, "this has a badword in it"))
	require.NoError(t, err)
	assert.Equal(t, bedrockruntime.GuardrailActionGuardrailIntervened, aws.StringValue(out.Action))
	require.Len(t, out.Outputs, 1)
	assert.Equal(t, "Your input violates our policy.", aws.StringValue(out.Outputs[0].Text))
	require.Len(t, out.Assessments, 1)
	require.NotNil(t, out.Usage)
}

// TestApplyGuardrail_OutputAnonymize covers OUTPUT content redaction: the PII
// entity configured ANONYMIZE, so the text comes back transformed rather than
// replaced by the blocked-outputs message.
func TestApplyGuardrail_OutputAnonymize(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("apply-guardrail-anonymize"))
	require.NoError(t, err)

	out, err := ApplyGuardrail(ctx, grCallerAccount, store, applyGuardrailInput(createOut.GuardrailId, guardrailDraftVersion, bedrockruntime.GuardrailContentSourceOutput, "contact jane@example.com for support"))
	require.NoError(t, err)
	assert.Equal(t, bedrockruntime.GuardrailActionNone, aws.StringValue(out.Action))
	require.Len(t, out.Outputs, 1)
	assert.Equal(t, "contact {EMAIL} for support", aws.StringValue(out.Outputs[0].Text))
}

// TestApplyGuardrail_UnknownOrForeignGuardrail covers both a guardrail id
// that was never created and one that belongs to another account.
func TestApplyGuardrail_UnknownOrForeignGuardrail(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	_, err := ApplyGuardrail(ctx, grCallerAccount, store, applyGuardrailInput(aws.String("does-not-exist"), guardrailDraftVersion, bedrockruntime.GuardrailContentSourceInput, "hello"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("apply-guardrail-foreign"))
	require.NoError(t, err)

	_, err = ApplyGuardrail(ctx, grOtherCaller, store, applyGuardrailInput(createOut.GuardrailId, guardrailDraftVersion, bedrockruntime.GuardrailContentSourceInput, "hello"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestApplyGuardrail_VersionResolution covers version handling: DRAFT
// defaults available on the mutable copy, and a numbered version created via
// CreateGuardrailVersion resolves to its own frozen policy config.
func TestApplyGuardrail_VersionResolution(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("apply-guardrail-version"))
	require.NoError(t, err)

	verOut, err := CreateGuardrailVersion(ctx, grCallerAccount, store, &bedrock.CreateGuardrailVersionInput{GuardrailIdentifier: createOut.GuardrailId})
	require.NoError(t, err)

	// Mutate the DRAFT's word policy after the snapshot was taken.
	_, err = UpdateGuardrail(ctx, grCallerAccount, store, &bedrock.UpdateGuardrailInput{
		GuardrailIdentifier:     createOut.GuardrailId,
		Name:                    aws.String("apply-guardrail-version"),
		BlockedInputMessaging:   aws.String("Your input violates our policy."),
		BlockedOutputsMessaging: aws.String("The model response violates our policy."),
		WordPolicyConfig: &bedrock.GuardrailWordPolicyConfig{
			WordsConfig: []*bedrock.GuardrailWordConfig{{Text: aws.String("newword")}},
		},
	})
	require.NoError(t, err)

	// The numbered version still enforces the original "badword" blocklist.
	outSnap, err := ApplyGuardrail(ctx, grCallerAccount, store, applyGuardrailInput(createOut.GuardrailId, aws.StringValue(verOut.Version), bedrockruntime.GuardrailContentSourceInput, "this has a badword in it"))
	require.NoError(t, err)
	assert.Equal(t, bedrockruntime.GuardrailActionGuardrailIntervened, aws.StringValue(outSnap.Action))

	// The DRAFT no longer blocks "badword" but does block "newword".
	outDraftOld, err := ApplyGuardrail(ctx, grCallerAccount, store, applyGuardrailInput(createOut.GuardrailId, guardrailDraftVersion, bedrockruntime.GuardrailContentSourceInput, "this has a badword in it"))
	require.NoError(t, err)
	assert.Equal(t, bedrockruntime.GuardrailActionNone, aws.StringValue(outDraftOld.Action))

	outDraftNew, err := ApplyGuardrail(ctx, grCallerAccount, store, applyGuardrailInput(createOut.GuardrailId, guardrailDraftVersion, bedrockruntime.GuardrailContentSourceInput, "this has a newword in it"))
	require.NoError(t, err)
	assert.Equal(t, bedrockruntime.GuardrailActionGuardrailIntervened, aws.StringValue(outDraftNew.Action))

	// An unknown numbered version is not-found.
	_, err = ApplyGuardrail(ctx, grCallerAccount, store, applyGuardrailInput(createOut.GuardrailId, "99", bedrockruntime.GuardrailContentSourceInput, "hello"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}
