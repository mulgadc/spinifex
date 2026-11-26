package ochre

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var listBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func putGuardrail(t *testing.T, store *GuardrailStore, account, id string, created time.Time) {
	t.Helper()
	rec := GuardrailRecord{
		guardrailView: guardrailView{Name: id, CreatedAt: created, UpdatedAt: created},
		ARN:           FormatGuardrailARN(ptTestRegion, account, id),
		GuardrailID:   id,
		AccountID:     account,
		Status:        bedrock.GuardrailStatusReady,
	}
	require.NoError(t, store.put(context.Background(), rec))
}

func putProvisioned(t *testing.T, store *ProvisionedStore, id, name string, created time.Time) {
	t.Helper()
	rec := ProvisionedModelRecord{
		ARN:                  FormatProvisionedModelARN(ptTestRegion, ptCallerAccount, id),
		ProvisionedModelName: name,
		ModelID:              selfHostTestModel,
		AccountID:            ptCallerAccount,
		ModelUnits:           1,
		Status:               bedrock.ProvisionedModelStatusCreating,
		CreationTime:         created,
		LastModifiedTime:     created,
	}
	require.NoError(t, store.store.Set(context.Background(), provisionedKey(ptCallerAccount, id), &rec))
}

func TestListGuardrails_PageWalk(t *testing.T) {
	t.Parallel()
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	for i, id := range []string{"gc", "ga", "gb"} {
		putGuardrail(t, store, grCallerAccount, id, listBase.Add(time.Duration(i)*time.Hour))
	}

	var got []string
	input := &bedrock.ListGuardrailsInput{MaxResults: aws.Int64(2)}
	pages := 1
	for ; ; pages++ {
		require.Less(t, pages, 3)
		out, err := ListGuardrails(ctx, grCallerAccount, store, input)
		require.NoError(t, err)
		for _, g := range out.Guardrails {
			got = append(got, aws.StringValue(g.Id))
		}
		if out.NextToken == nil {
			break
		}
		require.Len(t, out.Guardrails, 2)
		input.NextToken = out.NextToken
	}
	assert.Equal(t, 2, pages)
	assert.Equal(t, []string{"gc", "ga", "gb"}, got, "creation order, every guardrail once")
}

func TestListGuardrails_IdentifierListsThatGuardrailsVersions(t *testing.T) {
	t.Parallel()
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	out, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("versioned"))
	require.NoError(t, err)
	_, err = CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("other"))
	require.NoError(t, err)
	for range 2 {
		_, err = CreateGuardrailVersion(ctx, grCallerAccount, store, &bedrock.CreateGuardrailVersionInput{GuardrailIdentifier: out.GuardrailId})
		require.NoError(t, err)
	}

	page1, err := ListGuardrails(ctx, grCallerAccount, store, &bedrock.ListGuardrailsInput{GuardrailIdentifier: out.GuardrailArn, MaxResults: aws.Int64(2)})
	require.NoError(t, err)
	require.NotNil(t, page1.NextToken)
	page2, err := ListGuardrails(ctx, grCallerAccount, store, &bedrock.ListGuardrailsInput{GuardrailIdentifier: out.GuardrailArn, NextToken: page1.NextToken})
	require.NoError(t, err)
	assert.Nil(t, page2.NextToken)

	var versions []string
	for _, g := range append(page1.Guardrails, page2.Guardrails...) {
		assert.Equal(t, aws.StringValue(out.GuardrailId), aws.StringValue(g.Id))
		versions = append(versions, aws.StringValue(g.Version))
	}
	assert.Equal(t, []string{"DRAFT", "1", "2"}, versions)

	_, err = ListGuardrails(ctx, grOtherCaller, store, &bedrock.ListGuardrailsInput{GuardrailIdentifier: out.GuardrailId})
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestListGuardrails_RejectsBadPaging(t *testing.T) {
	t.Parallel()
	store := newGuardrailTestStore(t)
	for _, input := range []*bedrock.ListGuardrailsInput{
		{NextToken: aws.String("not-a-token")},
		{MaxResults: aws.Int64(0)},
		{MaxResults: aws.Int64(1001)},
	} {
		_, err := ListGuardrails(context.Background(), grCallerAccount, store, input)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException), "%v", input)
	}
}

// The filtered-out "skip" sorts inside the first page, so a page cut before
// the filter would show as a short first page.
func TestListProvisionedModelThroughputs_FilteredDescendingPageWalk(t *testing.T) {
	t.Parallel()
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	ctx := context.Background()
	putProvisioned(t, store, "p1", "keep-1", listBase)
	putProvisioned(t, store, "p2", "keep-2", listBase.Add(time.Hour))
	putProvisioned(t, store, "p3", "skip", listBase.Add(2*time.Hour))
	putProvisioned(t, store, "p4", "keep-4", listBase.Add(3*time.Hour))

	var got []string
	input := &bedrock.ListProvisionedModelThroughputsInput{
		NameContains: aws.String("keep"),
		SortOrder:    aws.String(bedrock.SortOrderDescending),
		MaxResults:   aws.Int64(2),
	}
	pages := 1
	for ; ; pages++ {
		require.Less(t, pages, 3)
		out, err := ListProvisionedModelThroughputs(ctx, ptCallerAccount, store, input)
		require.NoError(t, err)
		for _, s := range out.ProvisionedModelSummaries {
			got = append(got, aws.StringValue(s.ProvisionedModelName))
		}
		if out.NextToken == nil {
			break
		}
		require.Len(t, out.ProvisionedModelSummaries, 2)
		input.NextToken = out.NextToken
	}
	assert.Equal(t, 2, pages)
	assert.Equal(t, []string{"keep-4", "keep-2", "keep-1"}, got)
}

func TestListProvisionedModelThroughputs_Filters(t *testing.T) {
	t.Parallel()
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	ctx := context.Background()
	putProvisioned(t, store, "p1", "a", listBase)
	putProvisioned(t, store, "p2", "b", listBase.Add(time.Hour))
	putProvisioned(t, store, "p3", "c", listBase.Add(2*time.Hour))

	names := func(input *bedrock.ListProvisionedModelThroughputsInput) []string {
		out, err := ListProvisionedModelThroughputs(ctx, ptCallerAccount, store, input)
		require.NoError(t, err)
		names := []string{}
		for _, s := range out.ProvisionedModelSummaries {
			names = append(names, aws.StringValue(s.ProvisionedModelName))
		}
		return names
	}
	assert.Equal(t, []string{"b"}, names(&bedrock.ListProvisionedModelThroughputsInput{
		CreationTimeAfter: aws.Time(listBase), CreationTimeBefore: aws.Time(listBase.Add(2 * time.Hour)),
	}))
	assert.Empty(t, names(&bedrock.ListProvisionedModelThroughputsInput{StatusEquals: aws.String(bedrock.ProvisionedModelStatusInService)}))
	assert.Empty(t, names(&bedrock.ListProvisionedModelThroughputsInput{ModelArnEquals: aws.String(modelARN("other.model-v1"))}))
	assert.Equal(t, []string{"a", "b", "c"}, names(&bedrock.ListProvisionedModelThroughputsInput{ModelArnEquals: aws.String(modelARN(selfHostTestModel))}))
}

func TestListProvisionedModelThroughputs_RejectsBadInput(t *testing.T) {
	t.Parallel()
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	for _, input := range []*bedrock.ListProvisionedModelThroughputsInput{
		{NextToken: aws.String("not-a-token")},
		{MaxResults: aws.Int64(0)},
		{MaxResults: aws.Int64(1001)},
		{SortBy: aws.String("Name")},
		{SortOrder: aws.String("Sideways")},
		{StatusEquals: aws.String("Gone")},
	} {
		_, err := ListProvisionedModelThroughputs(context.Background(), ptCallerAccount, store, input)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException), "%v", input)
	}
}
