package awsapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each list route hands its folded query to the service: maxResults, nextToken
// and, for ListAccessEntries, associatedPolicyArn.
func TestListRoutes_PassPagingQuery(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	cases := []struct {
		action string
		params []string
		query  string
		want   map[string]any
	}{
		{"ListNodegroups", []string{"c1"}, `{"maxResults":["3"],"nextToken":["tok"]}`,
			map[string]any{"ClusterName": "c1", "MaxResults": 3.0, "NextToken": "tok"}},
		{"ListAddons", []string{"c1"}, `{"maxResults":["3"],"nextToken":["tok"]}`,
			map[string]any{"ClusterName": "c1", "MaxResults": 3.0, "NextToken": "tok"}},
		{"ListAccessPolicies", nil, `{"maxResults":["3"],"nextToken":["tok"]}`,
			map[string]any{"MaxResults": 3.0, "NextToken": "tok"}},
		{"ListAccessEntries", []string{"c1"}, `{"maxResults":["3"],"nextToken":["tok"],"associatedPolicyArn":["arn:p"]}`,
			map[string]any{"ClusterName": "c1", "MaxResults": 3.0, "NextToken": "tok", "AssociatedPolicyArn": "arn:p"}},
		{"ListAssociatedAccessPolicies", []string{"c1", "arn:u"}, `{"maxResults":["3"],"nextToken":["tok"]}`,
			map[string]any{"ClusterName": "c1", "PrincipalArn": "arn:u", "MaxResults": 3.0, "NextToken": "tok"}},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			got := make(chan map[string]any, 1)
			sub, err := nc.Subscribe("eks."+tc.action, func(m *nats.Msg) {
				var in map[string]any
				_ = json.Unmarshal(m.Data, &in)
				got <- in
				_ = m.Respond([]byte(`{}`))
			})
			require.NoError(t, err)
			defer func() { _ = sub.Unsubscribe() }()

			h := routeFor(t, tc.action)
			_, err = h(context.Background(), nc, acct, "", tc.params, []byte(tc.query))
			require.NoError(t, err)
			assert.Equal(t, tc.want, <-got)

			_, err = h(context.Background(), nc, acct, "", tc.params, []byte(`{"maxResults":["many"]}`))
			require.ErrorContains(t, err, "maxResults")
		})
	}
}

func routeFor(t *testing.T, action string) routeHandler {
	t.Helper()
	for _, r := range routes {
		if r.Action == action {
			return r.Handler
		}
	}
	t.Fatalf("no %s route", action)
	return nil
}
