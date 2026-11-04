package awsgw

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// platformCleaner builds the EC2 platform cleanup over the owners' own services
// in this process. It has no NATS subject, so the only caller is the operator
// admin DeleteAccount that holds it. Deleting a key pair, placement group or
// launch template through these owners clears its tag-index entry too.
func platformCleaner(ctx context.Context, node *config.Config, nc *nats.Conn, js jetstream.JetStream, nodeIDs []string) (accountlifecycle.PlatformCleaner, error) {
	tagsKV, err := ec2tags.GetOrCreateTagsBucket(ctx, js)
	if err != nil {
		return nil, fmt.Errorf("tags bucket: %w", err)
	}
	tags := ec2tags.NewTagsServiceImpl(node, tagsKV)
	keys := ec2key.NewKeyServiceImpl(node)
	groups, err := ec2placementgroup.NewPlacementGroupServiceImplWithNATS(ctx, node, nc)
	if err != nil {
		return nil, fmt.Errorf("placement group service: %w", err)
	}
	templates, err := ec2launchtemplate.NewLaunchTemplateServiceImplWithNATS(ctx, node, nc)
	if err != nil {
		return nil, fmt.Errorf("launch template service: %w", err)
	}
	spot, err := ec2spotinstance.NewSpotInstanceServiceImplWithNATS(ctx, node, nc)
	if err != nil {
		return nil, fmt.Errorf("spot instance service: %w", err)
	}
	keys.SetCentralTagStore(tags)
	groups.SetCentralTagStore(tags)
	templates.SetCentralTagStore(tags)

	return accountcleanup.New(keys, groups, templates, tags, accountcleanup.ReferenceReaders{
		Instances: accountlifecycle.NewInstanceReferenceReader(nc, nodeIDs),
		Spot:      spot,
		Templates: templates,
		Consumers: accountlifecycle.NewTemplateConsumerReader(nc),
	}), nil
}

// withPlatformCleaner binds the cleanup every DeleteAccount runs. When it cannot
// be built the field stays nil, which refuses the request instead of half-running it.
func withPlatformCleaner(d operatoradmin.Deps, build func() (accountlifecycle.PlatformCleaner, error)) operatoradmin.Deps {
	cleaner, err := build()
	if err != nil {
		slog.Error("awsgw: EC2 platform cleanup unavailable", "err", err)
		return d
	}
	d.PlatformCleaner = cleaner
	return d
}
