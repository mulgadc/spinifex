package accountlifecycle

//test:in-package — drives the engine over the real EC2 owners with the
// unexported inventory seam.

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountinventory"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type referencedByInstance struct{ keys, groups []string }

func (r referencedByInstance) AccountInstanceReferences(context.Context, string) (*accountcleanup.References, error) {
	refs := &accountcleanup.References{KeyNames: map[string]bool{}, PlacementGroupNames: map[string]bool{}, PlacementGroupIDs: map[string]bool{}}
	for _, k := range r.keys {
		refs.KeyNames[k] = true
	}
	for _, g := range r.groups {
		refs.PlacementGroupNames[g] = true
	}
	return refs, nil
}

type noSpotRequests struct{}

func (noSpotRequests) AccountLaunchReferences(context.Context, string) (*ec2spotinstance.LaunchReferences, error) {
	return &ec2spotinstance.LaunchReferences{}, nil
}

type namedByNodegroup struct{ id string }

func (n namedByNodegroup) AccountTemplateConsumers(context.Context, string) (*accountcleanup.TemplateConsumers, error) {
	return &accountcleanup.TemplateConsumers{TemplateIDs: map[string]bool{n.id: true}, TemplateNames: map[string]bool{}}, nil
}

type ownerInventory struct{ inv *accountinventory.Inventory }

func (o ownerInventory) PlatformInventory(ctx context.Context, accountID, correlationRef string) ec2v1.PlatformInventoryReply {
	reply, err := o.inv.PlatformInventory(ctx, &ec2v1.PlatformInventoryRequest{AccountID: accountID, CorrelationRef: correlationRef})
	if err != nil {
		return ec2v1.IncompletePlatformInventory(accountID, correlationRef, ec2v1.PlatformReasonInvalidReply)
	}
	return *reply
}

// ADR-0003:S7: Force on an ordinary delete cannot bypass a reference check. A
// key pair named by an instance, a placement group with a member and a template
// named by a nodegroup are all kept, and the account stays TERMINATING.
func TestS7_ForceNeverBypassesAReference(t *testing.T) {
	o := newPlatformOwners(t)
	ctx := testCtx(t)
	_, err := o.keys.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
		KeyName:           aws.String("in-use"),
		PublicKeyMaterial: []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"),
	}, testAccountID)
	require.NoError(t, err)
	pg, err := o.groups.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("held"), Strategy: aws.String(ec2.PlacementStrategyCluster),
	}, testAccountID)
	require.NoError(t, err)
	reserved, err := o.groups.ReserveClusterNode(ctx, &ec2placementgroup.ReserveClusterNodeInput{GroupName: "held", EligibleNodes: []string{"node-1"}}, testAccountID)
	require.NoError(t, err)
	_, err = o.groups.FinalizeClusterInstances(ctx, &ec2placementgroup.FinalizeClusterInstancesInput{
		GroupName: "held", NodeInstances: map[string][]string{reserved.TargetNode: {"i-0123456789abcdef0"}},
	}, testAccountID)
	require.NoError(t, err)
	lt, err := o.templates.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("nodes"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{ImageId: aws.String("ami-123")},
	}, testAccountID)
	require.NoError(t, err)
	ltID := aws.StringValue(lt.LaunchTemplate.LaunchTemplateId)

	cleaner := accountcleanup.New(o.keys, o.groups, o.templates, o.tags, accountcleanup.ReferenceReaders{
		Instances: referencedByInstance{keys: []string{"in-use"}},
		Spot:      noSpotRequests{},
		Templates: o.templates,
		Consumers: namedByNodegroup{id: ltID},
	})
	accounts := newFakeAccounts()
	engine := exerciseEngine(accounts, cleaner, ownerInventory{inv: accountinventory.New(o.keys, o.groups, o.templates, o.tags)})
	req := exerciseRequest()
	req.Force = true

	_, err = engine.Teardown(ctx, req)

	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.False(t, accounts.deleted)
	assert.Equal(t, AccountStatusTerminating, accounts.account.Status)
	keys, err := o.keys.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, keys.KeyPairs, 1)
	groups, err := o.groups.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, groups.PlacementGroups, 1)
	assert.NotEmpty(t, aws.StringValue(pg.PlacementGroup.GroupId))
	templates, err := o.templates.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, templates.LaunchTemplates, 1)
}
