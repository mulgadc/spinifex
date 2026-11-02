package message_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forbiddenConsumerState names every JetStream mechanism ADR-0011:S5 bars
// from SQS state: acknowledgement and redelivery state, durable or
// configured consumers, and retention that deletes on acknowledgement.
var forbiddenConsumerState = map[string]string{
	"AckWait":                "per-consumer redelivery timer",
	"MaxDeliver":             "consumer delivery limit",
	"MaxAckPending":          "consumer in-flight limit",
	"NumDelivered":           "consumer delivery count",
	"NakWithDelay":           "consumer redelivery",
	"Nak":                    "consumer redelivery",
	"InProgress":             "consumer AckWait reset",
	"Ack":                    "consumer acknowledgement",
	"AckSync":                "consumer acknowledgement",
	"DoubleAck":              "consumer acknowledgement",
	"ManualAck":              "acknowledged subscription",
	"AckAll":                 "acknowledged subscription",
	"AckExplicit":            "acknowledged subscription",
	"Term":                   "consumer acknowledgement",
	"TermWithReason":         "consumer acknowledgement",
	"AckExplicitPolicy":      "acknowledged consumer",
	"AckAllPolicy":           "acknowledged consumer",
	"Durable":                "durable consumer",
	"CreateConsumer":         "configured consumer",
	"CreateOrUpdateConsumer": "configured consumer",
	"UpdateConsumer":         "configured consumer",
	"PushConsumer":           "configured consumer",
	"WorkQueuePolicy":        "retention that deletes on acknowledgement",
	"InterestPolicy":         "retention that deletes on acknowledgement",
}

// TestS5_NoConsumerDeliveryStateInSQS holds ADR-0011:S5 across every
// non-test Go file of the SQS domain: the only consumer allowed is the
// ephemeral, unacknowledged ordered consumer.
func TestS5_NoConsumerDeliveryStateInSQS(t *testing.T) {
	root := ".."
	var scanned int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		scanned++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			var name string
			switch n := n.(type) {
			case *ast.SelectorExpr:
				name = n.Sel.Name
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok {
					name = id.Name
				}
			default:
				return true
			}
			if why, bad := forbiddenConsumerState[name]; bad {
				assert.Failf(t, "ADR-0011:S5 breach",
					"%s uses %s (%s); \"JetStream consumer delivery state never defines an SQS semantic\". Keep the state in the domain's conditional writes and use only the ordered consumer.",
					fset.Position(n.Pos()), name, why)
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, scanned, "no SQS source found under %s", root)
}
