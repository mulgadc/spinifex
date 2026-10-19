package accountcleanup

import (
	"context"

	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
)

// References are the key-pair and placement-group names, and placement-group
// IDs, that other EC2 records still name. They select what to keep and are
// never reported.
type References struct {
	KeyNames            map[string]bool
	PlacementGroupNames map[string]bool
	PlacementGroupIDs   map[string]bool
}

// InstanceReferenceReader reads the key-pair and placement-group names the
// account's non-terminated instance records carry, stopped ones included. It
// is complete or an error, never a partial answer. A later wiring supplies it.
type InstanceReferenceReader interface {
	AccountInstanceReferences(ctx context.Context, accountID string) (*References, error)
}

// SpotReferenceReader reads the names the account's open or active spot
// requests would launch with, complete or an error.
type SpotReferenceReader interface {
	AccountLaunchReferences(ctx context.Context, accountID string) (*ec2spotinstance.LaunchReferences, error)
}

// TemplateReferenceReader reads the names and IDs every version of every
// live launch template would launch with, complete or an error.
type TemplateReferenceReader interface {
	AccountTemplateReferences(ctx context.Context, accountID string) (*ec2launchtemplate.TemplateReferences, error)
}

var (
	_ SpotReferenceReader     = (*ec2spotinstance.SpotInstanceServiceImpl)(nil)
	_ TemplateReferenceReader = (*ec2launchtemplate.LaunchTemplateServiceImpl)(nil)
)

// ReferenceReaders are the checks run before key pairs and placement groups
// are deleted. A nil reader counts as a failed check.
type ReferenceReaders struct {
	Instances InstanceReferenceReader
	Spot      SpotReferenceReader
	Templates TemplateReferenceReader
}

func newReferences() *References {
	return &References{KeyNames: map[string]bool{}, PlacementGroupNames: map[string]bool{}, PlacementGroupIDs: map[string]bool{}}
}

func (r *References) addKeys(names ...string) {
	for _, name := range names {
		if name != "" {
			r.KeyNames[name] = true
		}
	}
}

func (r *References) addGroups(names ...string) {
	for _, name := range names {
		if name != "" {
			r.PlacementGroupNames[name] = true
		}
	}
}

func (r *References) addGroupIDs(ids ...string) {
	for _, id := range ids {
		if id != "" {
			r.PlacementGroupIDs[id] = true
		}
	}
}

func (r *References) merge(o *References) {
	for name := range o.KeyNames {
		r.addKeys(name)
	}
	for name := range o.PlacementGroupNames {
		r.addGroups(name)
	}
	for id := range o.PlacementGroupIDs {
		r.addGroupIDs(id)
	}
}

func (r *References) hasGroup(id, name string) bool {
	return r.PlacementGroupIDs[id] || r.PlacementGroupNames[name]
}
