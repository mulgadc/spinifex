package reqctx_test

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/stretchr/testify/assert"
)

func TestEmptyContextCarriesNothing(t *testing.T) {
	ctx := context.Background()
	_, ok := reqctx.Principal(ctx)
	assert.False(t, ok)
	assert.Empty(t, reqctx.Service(ctx))
	assert.Empty(t, reqctx.Region(ctx))
	assert.Empty(t, reqctx.Action(ctx))
	assert.Empty(t, reqctx.AccessKeyID(ctx))
}

func TestWritesAccumulateInOneRecord(t *testing.T) {
	caller := credential.Principal{Identity: "alice", AccountID: "123456789012", PrincipalType: "user"}
	ctx := reqctx.WithService(context.Background(), "ec2")
	ctx = reqctx.WithRegion(ctx, "ap-southeast-2")
	ctx = reqctx.WithAction(ctx, "DescribeVpcs")
	ctx = reqctx.WithPrincipal(ctx, caller, "AKIAEXAMPLE")

	got, ok := reqctx.Principal(ctx)
	assert.True(t, ok)
	assert.Equal(t, caller, got)
	assert.Equal(t, "AKIAEXAMPLE", reqctx.AccessKeyID(ctx))
	assert.Equal(t, "ec2", reqctx.Service(ctx))
	assert.Equal(t, "ap-southeast-2", reqctx.Region(ctx))
	assert.Equal(t, "DescribeVpcs", reqctx.Action(ctx))
}

// A write is visible to the context it returns and to its descendants, never to
// the context it was derived from.
func TestWriteIsNotVisibleUpstream(t *testing.T) {
	parent := reqctx.WithService(context.Background(), "ec2")
	child := reqctx.WithAction(parent, "RunInstances")

	assert.Empty(t, reqctx.Action(parent))
	assert.Equal(t, "RunInstances", reqctx.Action(child))
	assert.Equal(t, "ec2", reqctx.Service(child))
}

// The action label may be refined after it is first recorded.
func TestActionMayBeRefined(t *testing.T) {
	ctx := reqctx.WithAction(context.Background(), "Query")
	ctx = reqctx.WithAction(ctx, "ListClusters")
	assert.Equal(t, "ListClusters", reqctx.Action(ctx))
}
