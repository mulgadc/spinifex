package bodyscope_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/gateway/bodyscope"
	"github.com/stretchr/testify/assert"
)

func TestParse_LookupIsCaseInsensitive(t *testing.T) {
	scope := bodyscope.Parse("DeleteCluster", []byte(`{"Cluster":"prod"}`))
	assert.Equal(t, "prod", scope.String("cluster"))
	assert.Equal(t, "prod", scope.String("CLUSTER"))
}

func TestParse_FirstNamePresentWins(t *testing.T) {
	scope := bodyscope.Parse("StopTask", []byte(`{"task":"t-1","taskId":"t-2"}`))
	assert.Equal(t, "t-1", scope.String("task", "taskId"))
	assert.Equal(t, "t-2", scope.String("taskId", "task"))
}

// The reason for map[string]json.RawMessage over a shared struct: a mismatch on
// a field the scope does not read must not widen the request to "*".
func TestParse_UnrelatedFieldTypeMismatchDoesNotPoisonTheParse(t *testing.T) {
	scope := bodyscope.Parse("RunTask", []byte(`{"cluster":"prod","count":"not-a-number"}`))
	assert.Equal(t, "prod", scope.String("cluster"))
}

func TestParse_WrongShapeFieldIsSkipped(t *testing.T) {
	scope := bodyscope.Parse("DescribeClusters", []byte(`{"clusters":"prod"}`))
	assert.Empty(t, scope.Strings("clusters"))
	assert.Empty(t, bodyscope.Parse("x", []byte(`{"cluster":["prod"]}`)).String("cluster"))
}

func TestStrings_DropsEmptyElements(t *testing.T) {
	scope := bodyscope.Parse("DescribeClusters", []byte(`{"clusters":["prod","","dev"]}`))
	assert.Equal(t, []string{"prod", "dev"}, scope.Strings("clusters"))
}

// A body the gate cannot read authorizes account-wide; the handler still
// rejects it, so the caller keeps its validation fault.
func TestParse_UnparseableAndEmptyBodiesResolveToNothing(t *testing.T) {
	for _, body := range []string{"", "{not json", "[]", "null"} {
		scope := bodyscope.Parse("CreateCluster", []byte(body))
		assert.Empty(t, scope.String("clusterName"), "body %q", body)
		assert.Empty(t, scope.Strings("clusters"), "body %q", body)
		assert.False(t, scope.Has("clusterName"), "body %q", body)
	}
}

func TestObject_ReachesANestedIdentifier(t *testing.T) {
	body := []byte(`{"retrieveAndGenerateConfiguration":{"knowledgeBaseConfiguration":{"knowledgeBaseId":"kb-1"}}}`)
	scope := bodyscope.Parse("RetrieveAndGenerate", body)
	nested := scope.Object("retrieveAndGenerateConfiguration").Object("knowledgeBaseConfiguration")
	assert.Equal(t, "kb-1", nested.String("knowledgeBaseId"))
}

func TestObject_MissingOrWrongShapeYieldsAnEmptyScope(t *testing.T) {
	scope := bodyscope.Parse("RetrieveAndGenerate", []byte(`{"config":"not-an-object"}`))
	assert.Empty(t, scope.Object("config").String("knowledgeBaseId"))
	assert.Empty(t, scope.Object("absent").String("knowledgeBaseId"))
}

func TestHas_ReportsPresenceWhateverTheShape(t *testing.T) {
	scope := bodyscope.Parse("RunTask", []byte(`{"cluster":null,"count":3}`))
	assert.True(t, scope.Has("cluster"))
	assert.True(t, scope.Has("count"))
	assert.False(t, scope.Has("group"))
}
