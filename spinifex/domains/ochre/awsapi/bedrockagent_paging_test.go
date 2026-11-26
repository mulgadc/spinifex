//test:in-package
package awsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockagent"
	ochrevector "github.com/mulgadc/spinifex/spinifex/domains/ochre/vector"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bedrockAgentListPage is the paged part of every bedrock-agent List output.
type bedrockAgentListPage struct {
	NextToken              *string                              `json:"nextToken"`
	KnowledgeBaseSummaries []*bedrockagent.KnowledgeBaseSummary `json:"knowledgeBaseSummaries"`
	DataSourceSummaries    []*bedrockagent.DataSourceSummary    `json:"dataSourceSummaries"`
	IngestionJobSummaries  []*bedrockagent.IngestionJobSummary  `json:"ingestionJobSummaries"`
}

// walkBedrockAgentList follows nextToken from the first page to the last at
// maxResults per page, requires every page but the last to be full, and
// returns the ids in page order.
func walkBedrockAgentList(t *testing.T, gw *testGateway, path string, maxResults int, id func(bedrockAgentListPage) []string) []string {
	t.Helper()
	var ids []string
	token := ""
	for range 10 {
		body := fmt.Sprintf(`{"maxResults": %d}`, maxResults)
		if token != "" {
			body = fmt.Sprintf(`{"maxResults": %d, "nextToken": %q}`, maxResults, token)
		}
		w := httptest.NewRecorder()
		require.NoError(t, gw.BedrockAgent_Request(w, bedrockAgentRequestWithAccount(http.MethodPost, path, body)))
		var page bedrockAgentListPage
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
		got := id(page)
		require.LessOrEqual(t, len(got), maxResults, "a page must not exceed maxResults")
		ids = append(ids, got...)
		if page.NextToken == nil {
			return ids
		}
		require.Len(t, got, maxResults, "a page before the last must be full")
		token = *page.NextToken
	}
	t.Fatal("listing did not end")
	return nil
}

// requireBedrockAgentListRefused requires body to be refused with ValidationException.
func requireBedrockAgentListRefused(t *testing.T, gw *testGateway, path string) {
	t.Helper()
	for _, body := range []string{`{"nextToken": "not-a-token"}`, `{"maxResults": 0}`, `{"maxResults": 1001}`} {
		err := gw.BedrockAgent_Request(httptest.NewRecorder(), bedrockAgentRequestWithAccount(http.MethodPost, path, body))
		require.Error(t, err, body)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException), body)
	}
}

func kbSummaryIDs(p bedrockAgentListPage) []string {
	var ids []string
	for _, s := range p.KnowledgeBaseSummaries {
		ids = append(ids, aws.StringValue(s.KnowledgeBaseId))
	}
	return ids
}

func dsSummaryIDs(p bedrockAgentListPage) []string {
	var ids []string
	for _, s := range p.DataSourceSummaries {
		ids = append(ids, aws.StringValue(s.DataSourceId))
	}
	return ids
}

func jobSummaryIDs(p bedrockAgentListPage) []string {
	var ids []string
	for _, s := range p.IngestionJobSummaries {
		ids = append(ids, aws.StringValue(s.IngestionJobId))
	}
	return ids
}

func TestBedrockAgentRequest_ListKnowledgeBases_Pages(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})
	var want []string
	for range 3 {
		want = append(want, createTestKnowledgeBase(t, gw))
	}

	assert.Equal(t, want, walkBedrockAgentList(t, gw, "/knowledgebases/", 2, kbSummaryIDs))
	requireBedrockAgentListRefused(t, gw, "/knowledgebases/")
}

func TestBedrockAgentRequest_ListDataSources_Pages(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})
	kbID := createTestKnowledgeBase(t, gw)
	otherKB := createTestKnowledgeBase(t, gw)
	want := []string{createTestDataSource(t, gw, kbID)}
	createTestDataSource(t, gw, otherKB) // sorts inside the first page, but belongs to another knowledge base
	want = append(want, createTestDataSource(t, gw, kbID), createTestDataSource(t, gw, kbID))

	path := "/knowledgebases/" + kbID + "/datasources/"
	assert.Equal(t, want, walkBedrockAgentList(t, gw, path, 2, dsSummaryIDs))
	requireBedrockAgentListRefused(t, gw, path)
}

// newIngestionJobListFixture makes a knowledge base and data source, and
// returns the list path and a job builder bound to them.
func newIngestionJobListFixture(t *testing.T, vector *fakeBedrockAgentVectorService) (*testGateway, string, string, func(id, state string, minute int) ochrevector.JobRecord) {
	t.Helper()
	gw := newBedrockAgentRequestGateway(t, vector)
	kbID := createTestKnowledgeBase(t, gw)
	dsID := createTestDataSource(t, gw, kbID)
	kbRec, err := gw.KB.Get(context.Background(), bedrockAgentTestAccount, kbID)
	require.NoError(t, err)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	job := func(id, state string, minute int) ochrevector.JobRecord {
		at := base.Add(time.Duration(minute) * time.Minute)
		return ochrevector.JobRecord{ID: id, IndexID: kbRec.IndexID, DataSourceID: dsID, State: state, CreatedAt: at, UpdatedAt: at}
	}
	return gw, "/knowledgebases/" + kbID + "/datasources/" + dsID + "/ingestionjobs/", kbRec.IndexID, job
}

func TestBedrockAgentRequest_ListIngestionJobs_Pages(t *testing.T) {
	vector := &fakeBedrockAgentVectorService{}
	gw, path, indexID, job := newIngestionJobListFixture(t, vector)
	foreign := job("job-0-foreign", ochrevector.JobStateReady, 0)
	foreign.DataSourceID = "ds-other"
	vector.listJobsResp.Jobs = []ochrevector.JobRecord{
		job("job-3", ochrevector.JobStateReady, 3), foreign,
		job("job-1", ochrevector.JobStateReady, 1), job("job-2", ochrevector.JobStateReady, 2),
	}

	assert.Equal(t, []string{"job-1", "job-2", "job-3"}, walkBedrockAgentList(t, gw, path, 2, jobSummaryIDs))
	require.NotNil(t, vector.listJobsReq)
	assert.Equal(t, indexID, vector.listJobsReq.IndexID, "the vector service lists only the bound index")
	assert.NotEmpty(t, vector.listJobsReq.DataSourceID, "the vector service lists only the data source")
	requireBedrockAgentListRefused(t, gw, path)
}

func TestBedrockAgentRequest_ListIngestionJobs_FilterAndSort(t *testing.T) {
	vector := &fakeBedrockAgentVectorService{}
	gw, path, _, job := newIngestionJobListFixture(t, vector)
	vector.listJobsResp.Jobs = []ochrevector.JobRecord{
		job("job-1", ochrevector.JobStateReady, 1), job("job-2", ochrevector.JobStateFailed, 2),
		job("job-3", ochrevector.JobStateRunning, 3), job("job-4", ochrevector.JobStateReady, 4),
	}
	filter := `"filters": [{"attribute": "STATUS", "operator": "EQ", "values": ["COMPLETE", "FAILED"]}]`

	for _, tc := range []struct {
		sortBy string
		want   []string
	}{
		{`{"attribute": "STARTED_AT", "order": "DESCENDING"}`, []string{"job-4", "job-2", "job-1"}},
		{`{"attribute": "STATUS", "order": "ASCENDING"}`, []string{"job-1", "job-4", "job-2"}},
	} {
		body := `{` + filter + `, "sortBy": ` + tc.sortBy + `}`
		w := httptest.NewRecorder()
		require.NoError(t, gw.BedrockAgent_Request(w, bedrockAgentRequestWithAccount(http.MethodPost, path, body)))
		var page bedrockAgentListPage
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
		assert.Equal(t, tc.want, jobSummaryIDs(page), tc.sortBy)
	}

	err := gw.BedrockAgent_Request(httptest.NewRecorder(), bedrockAgentRequestWithAccount(http.MethodPost, path,
		`{"filters": [{"attribute": "NAME", "operator": "EQ", "values": ["x"]}]}`))
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException))
}
