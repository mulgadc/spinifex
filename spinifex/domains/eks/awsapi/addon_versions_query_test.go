package awsapi

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureDescribeAddonVersions answers eks.DescribeAddonVersions and returns a
// function reading the last input the gateway sent to the service.
func captureDescribeAddonVersions(t *testing.T, nc *nats.Conn) func() eks.DescribeAddonVersionsInput {
	t.Helper()
	var (
		mu   sync.Mutex
		last eks.DescribeAddonVersionsInput
	)
	sub, err := nc.Subscribe("eks.DescribeAddonVersions", func(m *nats.Msg) {
		mu.Lock()
		last = eks.DescribeAddonVersionsInput{}
		_ = json.Unmarshal(m.Data, &last)
		mu.Unlock()
		_ = m.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return func() eks.DescribeAddonVersionsInput {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// The gateway folds a GET query string into the body as a JSON object of
// string lists; the model binds every field below from the query.
func TestDescribeAddonVersions_PassesEveryModelQueryParameter(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	got := captureDescribeAddonVersions(t, nc)

	query := []byte(`{"addonName":["aws-ebs-csi-driver"],"kubernetesVersion":["1.32"],` +
		`"maxResults":["2"],"nextToken":["tok"],"types":["storage","networking"],` +
		`"publishers":["eks"],"owners":["aws"]}`)
	_, err := DescribeAddonVersions(context.Background(), nc, acct, query)
	require.NoError(t, err)

	in := got()
	assert.Equal(t, "aws-ebs-csi-driver", aws.StringValue(in.AddonName))
	assert.Equal(t, "1.32", aws.StringValue(in.KubernetesVersion))
	assert.Equal(t, int64(2), aws.Int64Value(in.MaxResults))
	assert.Equal(t, "tok", aws.StringValue(in.NextToken))
	assert.Equal(t, []string{"storage", "networking"}, aws.StringValueSlice(in.Types))
	assert.Equal(t, []string{"eks"}, aws.StringValueSlice(in.Publishers))
	assert.Equal(t, []string{"aws"}, aws.StringValueSlice(in.Owners))
}

func TestDescribeAddonVersions_NoQueryLeavesEveryFilterUnset(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	got := captureDescribeAddonVersions(t, nc)

	_, err := DescribeAddonVersions(context.Background(), nc, acct, nil)
	require.NoError(t, err)

	in := got()
	assert.Nil(t, in.AddonName)
	assert.Nil(t, in.KubernetesVersion)
	assert.Nil(t, in.MaxResults)
	assert.Nil(t, in.NextToken)
	assert.Empty(t, in.Types)
}

func TestDescribeAddonVersions_RefusesMalformedQuery(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	captureDescribeAddonVersions(t, nc)

	_, err := DescribeAddonVersions(context.Background(), nc, acct, []byte(`{"maxResults":["many"]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxResults")

	_, err = DescribeAddonVersions(context.Background(), nc, acct, []byte(`not json`))
	require.Error(t, err)
}

// The route must hand the folded query to the wrapper, not drop it.
func TestDescribeAddonVersionsRoute_PassesTheQuery(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	got := captureDescribeAddonVersions(t, nc)

	for _, r := range routes {
		if r.Action != "DescribeAddonVersions" {
			continue
		}
		_, err := r.Handler(context.Background(), nc, acct, "", nil, []byte(`{"addonName":["argocd"]}`))
		require.NoError(t, err)
		assert.Equal(t, "argocd", aws.StringValue(got().AddonName))
		return
	}
	t.Fatal("no DescribeAddonVersions route")
}
