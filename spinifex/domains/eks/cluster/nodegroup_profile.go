package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	nodegroupProfilePrefix = "eks-"

	profileClusterTagKey   = "spinifex:eks-cluster"
	profileNodegroupTagKey = "spinifex:eks-nodegroup"
	profileIDTagKey        = "spinifex:eks-profile-id"
)

// nodeProfileForLaunch returns the ARN of the instance profile a worker of rec
// launches with. A record that names no profile of its own predates per-group
// profiles and keeps the role-named one it already uses.
func (s *EKSServiceImpl) nodeProfileForLaunch(accountID string, rec *NodegroupRecord) (string, error) {
	if rec.InstanceProfileName == "" {
		return s.ensureNodeInstanceProfile(accountID, rec.NodeRole)
	}
	return s.ensureNodegroupProfile(accountID, rec)
}

func nodegroupProfileTags(rec *NodegroupRecord) []*iam.Tag {
	return []*iam.Tag{
		{Key: aws.String(profileClusterTagKey), Value: aws.String(rec.ClusterName)},
		{Key: aws.String(profileNodegroupTagKey), Value: aws.String(rec.Name)},
		{Key: aws.String(profileIDTagKey), Value: aws.String(rec.InstanceProfileID)},
	}
}

// profileOwnedBy reports whether p carries the ownership tags of this exact
// incarnation of rec.
func profileOwnedBy(p *iam.InstanceProfile, rec *NodegroupRecord) bool {
	have := map[string]string{}
	for _, t := range p.Tags {
		have[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	for _, want := range nodegroupProfileTags(rec) {
		if have[aws.StringValue(want.Key)] != aws.StringValue(want.Value) {
			return false
		}
	}
	return true
}

func roleNameFromARN(roleARN string) string {
	return roleARN[strings.LastIndex(roleARN, "/")+1:]
}

// ensureNodegroupProfile finds or creates the instance profile recorded for
// rec, tagged as its own, and makes sure it holds the node role. Concurrent
// worker launches converge on the one profile.
func (s *EKSServiceImpl) ensureNodegroupProfile(accountID string, rec *NodegroupRecord) (string, error) {
	roleName, err := s.resolveNodeRoleName(accountID, rec.NodeRole)
	if err != nil {
		return "", err
	}
	name := rec.InstanceProfileName
	ens := s.iamEnsurer()

	got, err := ens.GetInstanceProfile(accountID, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
	if err != nil {
		if code, _ := awserrors.ResolveErrorCode(err); code != awserrors.ErrorIAMNoSuchEntity {
			return "", fmt.Errorf("get instance profile %q: %w", name, err)
		}
		_, cerr := ens.CreateInstanceProfile(accountID, &iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(name),
			Tags:                nodegroupProfileTags(rec),
		})
		if cerr != nil {
			// A racing launch may have created it first; re-read and converge.
			if code, _ := awserrors.ResolveErrorCode(cerr); code != awserrors.ErrorIAMEntityAlreadyExists {
				return "", fmt.Errorf("create instance profile %q: %w", name, cerr)
			}
		}
		got, err = ens.GetInstanceProfile(accountID, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
		if err != nil {
			return "", fmt.Errorf("re-get instance profile %q: %w", name, err)
		}
	}
	if !profileOwnedBy(got.InstanceProfile, rec) {
		return "", fmt.Errorf("instance profile %q exists but is not this nodegroup's", name)
	}
	return s.attachRoleToProfile(accountID, name, roleName,
		aws.StringValue(got.InstanceProfile.Arn), len(got.InstanceProfile.Roles) > 0)
}

// deleteNodegroupProfile removes the profile recorded for rec, and only that
// one: it must be recorded, tagged for this incarnation, hold exactly the node
// role and be named by no other record. Anything else is left alone.
func (s *EKSServiceImpl) deleteNodegroupProfile(ctx context.Context, accountID string, kv jetstream.KeyValue, rec *NodegroupRecord) error {
	name := rec.InstanceProfileName
	ens := s.iamEnsurer()
	if name == "" || ens == nil {
		return nil
	}
	got, err := ens.GetInstanceProfile(accountID, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
	if err != nil {
		if code, _ := awserrors.ResolveErrorCode(err); code == awserrors.ErrorIAMNoSuchEntity {
			return nil
		}
		return fmt.Errorf("get instance profile %q: %w", name, err)
	}
	p := got.InstanceProfile

	if !profileOwnedBy(p, rec) {
		slog.WarnContext(ctx, "eks: leaving instance profile whose ownership tags do not match its nodegroup",
			"cluster", rec.ClusterName, "nodegroup", rec.Name, "profile", name)
		return nil
	}
	nodeRole := roleNameFromARN(rec.NodeRole)
	if len(p.Roles) > 1 || (len(p.Roles) == 1 && aws.StringValue(p.Roles[0].RoleName) != nodeRole) {
		slog.WarnContext(ctx, "eks: leaving instance profile that holds a role other than the node role",
			"cluster", rec.ClusterName, "nodegroup", rec.Name, "profile", name)
		return nil
	}
	shared, err := s.profileNamedByOtherRecord(ctx, kv, rec)
	if err != nil {
		return err
	}
	if shared {
		slog.WarnContext(ctx, "eks: leaving instance profile another nodegroup record names",
			"cluster", rec.ClusterName, "nodegroup", rec.Name, "profile", name)
		return nil
	}

	if len(p.Roles) == 1 {
		_, err := ens.RemoveRoleFromInstanceProfile(accountID, &iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: aws.String(name), RoleName: aws.String(nodeRole),
		})
		if err != nil {
			if code, _ := awserrors.ResolveErrorCode(err); code != awserrors.ErrorIAMNoSuchEntity {
				return fmt.Errorf("remove role %q from instance profile %q: %w", nodeRole, name, err)
			}
		}
	}
	if _, err := ens.DeleteInstanceProfile(accountID, &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(name)}); err != nil {
		if code, _ := awserrors.ResolveErrorCode(err); code != awserrors.ErrorIAMNoSuchEntity {
			return fmt.Errorf("delete instance profile %q: %w", name, err)
		}
	}
	return nil
}

// profileNamedByOtherRecord reports whether any nodegroup record in the
// account other than rec names rec's profile.
func (s *EKSServiceImpl) profileNamedByOtherRecord(ctx context.Context, kv jetstream.KeyValue, rec *NodegroupRecord) (bool, error) {
	clusters, err := listClusterNames(ctx, kv)
	if err != nil {
		return false, fmt.Errorf("list clusters: %w", err)
	}
	for _, cluster := range clusters {
		recs, err := ListNodegroupRecords(ctx, kv, cluster)
		if err != nil {
			return false, fmt.Errorf("list nodegroups of %s: %w", cluster, err)
		}
		for _, other := range recs {
			if other.InstanceProfileName == rec.InstanceProfileName && (other.ClusterName != rec.ClusterName || other.Name != rec.Name) {
				return true, nil
			}
		}
	}
	return false, nil
}
