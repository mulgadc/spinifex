package awsapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bedrockListRegion = "us-east-1"

func TestBedrockRequest_ListGuardrailsPagesByQuery(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := ochre.NewGuardrailStore(js, bedrockListRegion)
	gw := &testGateway{Deps: Deps{Guardrails: store}, region: bedrockListRegion}
	want := map[string]bool{}
	var lastID string
	for _, name := range []string{"one", "two"} {
		out, err := ochre.CreateGuardrail(t.Context(), bedrockTestAccount, store, &bedrock.CreateGuardrailInput{
			Name: aws.String(name), BlockedInputMessaging: aws.String("no"), BlockedOutputsMessaging: aws.String("no"),
		})
		require.NoError(t, err)
		lastID = aws.StringValue(out.GuardrailId)
		want[lastID] = true
	}

	got := map[string]bool{}
	path := "/guardrails?maxResults=1"
	for pages := 0; ; pages++ {
		require.Less(t, pages, 2)
		w := httptest.NewRecorder()
		require.NoError(t, gw.Bedrock_Request(w, bedrockRequestWithAccount(http.MethodGet, path, "")))
		var out bedrock.ListGuardrailsOutput
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		require.Len(t, out.Guardrails, 1)
		got[aws.StringValue(out.Guardrails[0].Id)] = true
		if out.NextToken == nil {
			break
		}
		path = "/guardrails?maxResults=1&nextToken=" + url.QueryEscape(*out.NextToken)
	}
	assert.Equal(t, want, got)

	w := httptest.NewRecorder()
	require.NoError(t, gw.Bedrock_Request(w, bedrockRequestWithAccount(http.MethodGet, "/guardrails?guardrailIdentifier="+lastID, "")))
	var one bedrock.ListGuardrailsOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &one))
	require.Len(t, one.Guardrails, 1)
	assert.Equal(t, lastID, aws.StringValue(one.Guardrails[0].Id))

	for _, path := range []string{"/guardrails?nextToken=bogus", "/guardrails?maxResults=1001", "/guardrails?maxResults=x"} {
		err := gw.Bedrock_Request(httptest.NewRecorder(), bedrockRequestWithAccount(http.MethodGet, path, ""))
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException), path)
	}
}

func TestBedrockRequest_ListProvisionedModelThroughputsRefusesBadQuery(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	gw := &testGateway{Deps: Deps{Provisioned: ochre.NewProvisionedStore(js, bedrockListRegion, nil)}, region: bedrockListRegion}
	for _, q := range []string{"nextToken=bogus", "maxResults=0", "statusEquals=Gone", "sortOrder=Up", "creationTimeAfter=yesterday"} {
		err := gw.Bedrock_Request(httptest.NewRecorder(), bedrockRequestWithAccount(http.MethodGet, "/provisioned-model-throughputs?"+q, ""))
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException), q)
	}
}

func TestListProvisionedQuery_ReadsEveryInput(t *testing.T) {
	body, err := json.Marshal(map[string]string{
		"creationTimeAfter": "2026-01-01T00:00:00Z", "creationTimeBefore": "2026-02-01T00:00:00.5Z",
		"statusEquals": "InService", "modelArnEquals": "arn:m", "nameContains": "pt",
		"maxResults": "5", "nextToken": "tok", "sortBy": "CreationTime", "sortOrder": "Descending",
	})
	require.NoError(t, err)
	input, err := listProvisionedQuery(body)
	require.NoError(t, err)
	assert.Equal(t, &bedrock.ListProvisionedModelThroughputsInput{
		CreationTimeAfter:  aws.Time(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		CreationTimeBefore: aws.Time(time.Date(2026, 2, 1, 0, 0, 0, 5e8, time.UTC)),
		StatusEquals:       aws.String("InService"),
		ModelArnEquals:     aws.String("arn:m"),
		NameContains:       aws.String("pt"),
		MaxResults:         aws.Int64(5),
		NextToken:          aws.String("tok"),
		SortBy:             aws.String("CreationTime"),
		SortOrder:          aws.String("Descending"),
	}, input)
}
