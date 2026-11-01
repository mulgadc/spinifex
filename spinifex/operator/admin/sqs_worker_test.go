package admin

import (
	"testing"
	"time"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Missing or unsafe parameters are refused before any NATS request, so a nil
// connection is never reached.
func TestEnsureSQSWorker_RefusesBadParametersBeforeRequesting(t *testing.T) {
	nodes := []string{"node-1"}
	cases := []struct {
		name, node, typ, code string
	}{
		{"no node", "", "t3.micro", awserrors.ErrorMissingParameter},
		{"no type", "node-1", "", awserrors.ErrorMissingParameter},
		{"unknown node", "node-2", "t3.micro", awserrors.ErrorInvalidParameterValue},
		{"subject wildcard", "node-1.>", "t3.micro", awserrors.ErrorInvalidParameterValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EnsureSQSWorker(t.Context(), nil, nodes, tc.node, tc.typ, 8, "000000000001", time.Second)
			code, _, ok := awserrors.ResolveErrorDetail(err)
			if !ok || code != tc.code {
				t.Fatalf("got %v, want code %s", err, tc.code)
			}
		})
	}
}
