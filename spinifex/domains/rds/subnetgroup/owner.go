package subnetgroup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// How many times a modify replays against a fresh record after losing its CAS.
const modifyAttempts = 16

// Subnets is the EC2 read the group validates its members through, issued as the caller.
type Subnets interface {
	DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error)
}

// Dependants reports the sorted identifiers of the DB instances in the account that name the group.
type Dependants interface {
	InstancesUsing(ctx context.Context, accountID, name string) ([]string, error)
}

// Bucket opens the account's RDS bucket, which holds the group's records.
type Bucket func(ctx context.Context, accountID string) (*kvstore.Bucket, error)

// Owner is the sole writer of DB subnet group records.
type Owner struct {
	subnets    Subnets
	dependants Dependants
	bucket     Bucket
}

// New returns an Owner. A nil subnets refuses every create and modify, as on a node without RDS networking.
func New(subnets Subnets, dependants Dependants, bucket Bucket) *Owner {
	return &Owner{subnets: subnets, dependants: dependants, bucket: bucket}
}

// Spec is a validated create request.
type Spec struct {
	Name        string
	Description string
	SubnetIDs   []string
	Tags        map[string]string
}

// Create stores a new group, or fails with DBSubnetGroupAlreadyExists when the name is taken.
func (o *Owner) Create(ctx context.Context, accountID string, spec Spec) (*Record, error) {
	subnets, vpcID, err := o.resolveSubnets(ctx, accountID, spec.SubnetIDs)
	if err != nil {
		return nil, err
	}

	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := &Record{
		Name:        spec.Name,
		AccountID:   accountID,
		Description: spec.Description,
		Subnets:     subnets,
		VpcID:       vpcID,
		Tags:        spec.Tags,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// Create rather than Put, so two concurrent creates of one name have exactly
	// one winner instead of both reporting success over each other's subnets.
	if _, err := kvstore.On[Record](kv).Create(ctx, Key(spec.Name), rec); err != nil {
		if errors.Is(err, kvstore.ErrExists) {
			return nil, awserrors.Errorf(awserrors.ErrorDBSubnetGroupAlreadyExists,
				"DB subnet group %s already exists", spec.Name)
		}
		return nil, err
	}

	slog.InfoContext(ctx, "rds: DB subnet group created",
		"dbSubnetGroup", spec.Name, "accountId", accountID, "vpcId", vpcID, "subnets", len(subnets))
	return rec, nil
}

// Get returns the group, or DBSubnetGroupNotFoundFault.
func (o *Owner) Get(ctx context.Context, accountID, name string) (*Record, error) {
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, _, err := get(ctx, kv, name)
	return rec, err
}

// List returns every group in the account, sorted by name.
func (o *Owner) List(ctx context.Context, accountID string) ([]Record, error) {
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	recs, err := kvstore.On[Record](kv).List(ctx, Prefix())
	if err != nil {
		return nil, err
	}
	slices.SortFunc(recs, func(a, b Record) int { return strings.Compare(a.Name, b.Name) })
	return recs, nil
}

// Placement returns the VPC and member subnet IDs an instance created against the group may land in.
func (o *Owner) Placement(ctx context.Context, accountID, name string) (string, []string, error) {
	rec, err := o.Get(ctx, accountID, name)
	if err != nil {
		return "", nil, err
	}
	ids := make([]string, 0, len(rec.Subnets))
	for _, subnet := range rec.Subnets {
		ids = append(ids, subnet.SubnetID)
	}
	return rec.VpcID, ids, nil
}

// Modify replaces the group's subnet set, which must stay in the group's VPC, as AWS requires. An
// empty description keeps the stored one. A lost CAS replays against the fresh record, so a
// concurrent write is not undone.
func (o *Owner) Modify(ctx context.Context, accountID, name, description string, subnetIDs []string) (*Record, error) {
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	// Read first so a missing group is reported as such rather than as whatever
	// the subnet resolution would have objected to.
	if _, _, err := get(ctx, kv, name); err != nil {
		return nil, err
	}
	subnets, vpcID, err := o.resolveSubnets(ctx, accountID, subnetIDs)
	if err != nil {
		return nil, err
	}

	key := Key(name)
	for range modifyAttempts {
		rec, rev, err := get(ctx, kv, name)
		if err != nil {
			return nil, err
		}
		if vpcID != rec.VpcID {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"The new Subnets are not in the same Vpc as the existing subnet group")
		}
		if description != "" {
			rec.Description = description
		}
		rec.Subnets = subnets
		rec.UpdatedAt = time.Now().UTC()

		_, err = kvstore.On[Record](kv).CompareAndSet(ctx, key, rec, rev)
		if err == nil {
			slog.InfoContext(ctx, "rds: DB subnet group modified",
				"dbSubnetGroup", name, "accountId", accountID, "vpcId", vpcID, "subnets", len(subnets))
			return rec, nil
		}
		if !errors.Is(err, kvstore.ErrConflict) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("rds: update of %s contended after %d attempts", key, modifyAttempts)
}

// Delete is refused while any instance still names the group, including one that is only deleting:
// releasing it early would let a teardown lose the record of where its ENI was placed, and would
// make destroy ordering ambiguous.
func (o *Owner) Delete(ctx context.Context, accountID, name string) error {
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	if _, _, err := get(ctx, kv, name); err != nil {
		return err
	}
	users, err := o.dependants.InstancesUsing(ctx, accountID, name)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return awserrors.Errorf(awserrors.ErrorDBSubnetGroupInvalidState,
			"DB subnet group %s is still used by %s", name, strings.Join(users, ", "))
	}

	if err := kv.Delete(ctx, Key(name)); err != nil {
		return fmt.Errorf("rds: delete DB subnet group %s: %w", name, err)
	}
	slog.InfoContext(ctx, "rds: DB subnet group deleted", "dbSubnetGroup", name, "accountId", accountID)
	return nil
}

// The record plus its revision. A missing group raises AWS's own fault, so a
// well-formed name that resolves to nothing is distinguishable from a bad one.
func get(ctx context.Context, kv *kvstore.Bucket, name string) (*Record, uint64, error) {
	rec, rev, err := kvstore.On[Record](kv).Get(ctx, Key(name))
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil, 0, awserrors.Errorf(awserrors.ErrorDBSubnetGroupNotFound, "DB subnet group %s not found", name)
	}
	if err != nil {
		return nil, 0, err
	}
	return rec, rev, nil
}
