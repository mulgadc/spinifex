package accountlifecycle

//test:in-package — runs the real EC2 platform owners behind NATS subjects so
// the inventory adapter is tested against what they actually report.

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

const platformBucket = "test-bucket"

// platformOwners runs the real key, placement-group, launch-template and tag
// services behind the subjects the platform reapers call.
type platformOwners struct {
	nc        *nats.Conn
	store     *objectstore.MemoryObjectStore
	keys      *ec2key.KeyServiceImpl
	groups    *ec2placementgroup.PlacementGroupServiceImpl
	templates *ec2launchtemplate.LaunchTemplateServiceImpl
	tags      *ec2tags.TagsServiceImpl
	pgKV      jetstream.KeyValue
	ltKV      jetstream.KeyValue
	tagsKV    jetstream.KeyValue
}

// serveOwner answers subject with fn, taking the account from X-Account-ID
// as the daemon's handlers do.
func serveOwner[I, O any](t *testing.T, nc *nats.Conn, subject string, fn func(context.Context, *I, string) (*O, error)) {
	t.Helper()
	sub, err := nc.QueueSubscribe(subject, "spinifex-workers", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *I) (*O, error) {
			return fn(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func newPlatformOwners(t *testing.T) *platformOwners {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := t.Context()
	o := &platformOwners{nc: nc, store: objectstore.NewMemoryObjectStore()}

	var err error
	o.tagsKV, err = ec2tags.GetOrCreateTagsBucket(ctx, js)
	require.NoError(t, err)
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: platformBucket}}
	o.tags = ec2tags.NewTagsServiceImplWithStore(cfg, o.store, o.tagsKV)
	o.keys = ec2key.NewKeyServiceImplWithStore(o.store, platformBucket)
	o.keys.SetCentralTagStore(o.tags)
	o.groups, err = ec2placementgroup.NewPlacementGroupServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	o.groups.SetCentralTagStore(o.tags)
	o.templates, err = ec2launchtemplate.NewLaunchTemplateServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	o.templates.SetCentralTagStore(o.tags)
	o.pgKV, err = js.KeyValue(ctx, ec2placementgroup.KVBucketPlacementGroups)
	require.NoError(t, err)
	o.ltKV, err = js.KeyValue(ctx, ec2launchtemplate.KVBucketLaunchTemplates)
	require.NoError(t, err)

	serveOwner(t, nc, "ec2.DescribeKeyPairs", o.keys.DescribeKeyPairs)
	serveOwner(t, nc, "ec2.DeleteKeyPair", o.keys.DeleteKeyPair)
	serveOwner(t, nc, "ec2.DescribePlacementGroups", o.groups.DescribePlacementGroups)
	serveOwner(t, nc, "ec2.DeletePlacementGroup", o.groups.DeletePlacementGroup)
	serveOwner(t, nc, "ec2.DescribeLaunchTemplates", o.templates.DescribeLaunchTemplates)
	serveOwner(t, nc, "ec2.DeleteLaunchTemplate", o.templates.DeleteLaunchTemplate)
	require.NoError(t, nc.Flush())
	return o
}
