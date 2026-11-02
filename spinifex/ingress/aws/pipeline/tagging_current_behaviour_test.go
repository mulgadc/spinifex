package pipeline

import (
	"fmt"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"net/http"
	"strings"
	"testing"
)

// CreateTags refuses an aws: key but applies no tag-count, key-length or
// value-length limit: 51 tags, a 129-character key and a 257-character value
// are all stored.
func TestCurrentBehaviour_Tagging_EC2CreateTagsAppliesNoLengthOrCountLimits(t *testing.T) {
	gw, svc := newTaggingCBGateway(t)
	const sg = "sg-0a1b2c3d4e5f60718"

	tags := map[string]string{strings.Repeat("k", 129): strings.Repeat("v", 257)}
	for i := range 50 {
		tags[fmt.Sprintf("key-%02d", i)] = "v"
	}
	requireEC2OK(t, gw, ec2TagQuery("CreateTags", []string{sg}, tags))
	assert.Equal(t, 51, centralTagCount(t, svc, sg))

	w := serveEC2(gw, ec2TagQuery("CreateTags", []string{sg}, map[string]string{"aws:reserved": "v"}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>"+awserrors.ErrorInvalidParameterValue+"</Code>")
}
