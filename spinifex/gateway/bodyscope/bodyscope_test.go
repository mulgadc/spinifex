package bodyscope_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/gateway/bodyscope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParse(t *testing.T, action, body string) bodyscope.Scope {
	t.Helper()
	scope, err := bodyscope.Parse(action, []byte(body))
	require.NoError(t, err)
	return scope
}

func TestParse_LookupIsCaseInsensitive(t *testing.T) {
	scope := mustParse(t, "DeleteCluster", `{"Cluster":"prod"}`)
	assert.Equal(t, "prod", scope.String("cluster"))
	assert.Equal(t, "prod", scope.String("CLUSTER"))
}

func TestParse_FirstNamePresentWins(t *testing.T) {
	scope := mustParse(t, "StopTask", `{"task":"t-1","taskId":"t-2"}`)
	assert.Equal(t, "t-1", scope.String("task", "taskId"))
	assert.Equal(t, "t-2", scope.String("taskId", "task"))
}

// The reason for map[string]json.RawMessage over a shared struct: a mismatch on
// a field the scope does not read must not widen the request to "*".
func TestParse_UnrelatedFieldTypeMismatchDoesNotPoisonTheParse(t *testing.T) {
	scope := mustParse(t, "RunTask", `{"cluster":"prod","count":"not-a-number"}`)
	assert.Equal(t, "prod", scope.String("cluster"))
}

func TestParse_WrongShapeFieldIsSkipped(t *testing.T) {
	scope := mustParse(t, "DescribeClusters", `{"clusters":"prod"}`)
	assert.Empty(t, scope.Strings("clusters"))
	assert.Empty(t, mustParse(t, "x", `{"cluster":["prod"]}`).String("cluster"))
}

// encoding/json resolves two spellings of one field in document order when the
// handler builds its typed input; a case-folded map resolves them at random.
// The gate must refuse rather than name a different object than the handler.
func TestParse_FieldSpelledTwoWaysIsRejected(t *testing.T) {
	for _, body := range []string{
		`{"cluster":"dev","Cluster":"prod"}`,
		`{"repositoryName":"a","REPOSITORYNAME":"b"}`,
		`{"accountId":"1","AccountId":"2"}`,
	} {
		// Run repeatedly: the collision must be detected whichever order the
		// map yields, not on the runs where the fold happens to disagree.
		for range 50 {
			_, err := bodyscope.Parse("DeleteCluster", []byte(body))
			require.ErrorIs(t, err, bodyscope.ErrAmbiguousBody, "body %q", body)
		}
	}
}

func TestObject_FieldSpelledTwoWaysInANestedObjectIsRejected(t *testing.T) {
	body := `{"retrieveAndGenerateConfiguration":{"knowledgeBaseId":"kb-1","KnowledgeBaseId":"kb-2"}}`
	scope := mustParse(t, "RetrieveAndGenerate", body)
	_, err := scope.Object("retrieveAndGenerateConfiguration")
	require.ErrorIs(t, err, bodyscope.ErrAmbiguousBody)
}

// A field repeated under one spelling is not ambiguous: encoding/json takes the
// last occurrence and so does the map, so both sides agree.
func TestParse_RepeatedIdenticalSpellingIsNotAmbiguous(t *testing.T) {
	scope := mustParse(t, "DeleteCluster", `{"cluster":"dev","cluster":"prod"}`)
	assert.Equal(t, "prod", scope.String("cluster"))
}

func TestStrings_DropsEmptyElements(t *testing.T) {
	scope := mustParse(t, "DescribeClusters", `{"clusters":["prod","","dev"]}`)
	assert.Equal(t, []string{"prod", "dev"}, scope.Strings("clusters"))
}

// A body the gate cannot read authorizes account-wide; the handler still
// rejects it, so the caller keeps its validation fault.
func TestParse_UnparseableAndEmptyBodiesResolveToNothing(t *testing.T) {
	for _, body := range []string{"", "{not json", "[]", "null"} {
		scope, err := bodyscope.Parse("CreateCluster", []byte(body))
		require.NoError(t, err, "body %q", body)
		assert.Empty(t, scope.String("clusterName"), "body %q", body)
		assert.Empty(t, scope.Strings("clusters"), "body %q", body)
		assert.False(t, scope.Has("clusterName"), "body %q", body)
	}
}

func TestObject_ReachesANestedIdentifier(t *testing.T) {
	body := `{"retrieveAndGenerateConfiguration":{"knowledgeBaseConfiguration":{"knowledgeBaseId":"kb-1"}}}`
	scope := mustParse(t, "RetrieveAndGenerate", body)
	config, err := scope.Object("retrieveAndGenerateConfiguration")
	require.NoError(t, err)
	nested, err := config.Object("knowledgeBaseConfiguration")
	require.NoError(t, err)
	assert.Equal(t, "kb-1", nested.String("knowledgeBaseId"))
}

func TestObject_MissingOrWrongShapeYieldsAnEmptyScope(t *testing.T) {
	scope := mustParse(t, "RetrieveAndGenerate", `{"config":"not-an-object"}`)
	for _, name := range []string{"config", "absent"} {
		nested, err := scope.Object(name)
		require.NoError(t, err, "field %q", name)
		assert.Empty(t, nested.String("knowledgeBaseId"), "field %q", name)
	}
}

func TestHas_ReportsPresenceWhateverTheShape(t *testing.T) {
	scope := mustParse(t, "RunTask", `{"cluster":null,"count":3}`)
	assert.True(t, scope.Has("cluster"))
	assert.True(t, scope.Has("count"))
	assert.False(t, scope.Has("group"))
}
