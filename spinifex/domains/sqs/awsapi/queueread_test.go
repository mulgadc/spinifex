package awsapi

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gateway tests run on an IP endpoint, so a named host's case rule and a
// path-prefixed endpoint are checked here.
func TestParseQueueURL_NamedEndpoint(t *testing.T) {
	inv := dispatch.Invocation{AccountID: "123456789012"}

	for endpoint, forms := range map[string][]string{
		"https://sqs.example:9999":     {"https://sqs.example:9999/123456789012/q", "https://SQS.Example:9999/123456789012/q"},
		"https://edge.example/sqs-api": {"https://edge.example/sqs-api/123456789012/q"},
	} {
		for _, raw := range forms {
			name, err := parseQueueURL(Deps{Endpoint: endpoint}, inv, raw)
			require.NoError(t, err, raw)
			assert.Equal(t, "q", name)
		}
	}

	for _, raw := range []string{
		"https://edge.example/123456789012/q",
		"https://edge.example/other/123456789012/q",
	} {
		_, err := parseQueueURL(Deps{Endpoint: "https://edge.example/sqs-api"}, inv, raw)
		assert.Error(t, err, raw)
	}
}
