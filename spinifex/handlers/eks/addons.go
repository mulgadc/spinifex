package handlers_eks

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/addon"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
)

// ListAddons returns the names of every managed add-on installed on a cluster.
func (s *EKSServiceImpl) ListAddons(ctx context.Context, input *eks.ListAddonsInput, accountID string) (*eks.ListAddonsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	acctKV, err := s.acctKVForCluster(ctx, accountID, cluster)
	if err != nil {
		return nil, err
	}
	recs, err := addon.List(ctx, acctKV, cluster)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(recs))
	for _, rec := range recs {
		names = append(names, rec.AddonName)
	}
	return &eks.ListAddonsOutput{Addons: aws.StringSlice(names)}, nil
}

// DescribeAddonVersions returns the static add-on catalog, optionally filtered by name.
func (s *EKSServiceImpl) DescribeAddonVersions(ctx context.Context, input *eks.DescribeAddonVersionsInput, _ string) (*eks.DescribeAddonVersionsOutput, error) {
	filter := ""
	if input != nil {
		filter = aws.StringValue(input.AddonName)
	}
	specs := addon.Specs()
	out := make([]*eks.AddonInfo, 0, len(specs))
	for _, spec := range specs {
		if filter != "" && spec.Name != filter {
			continue
		}
		// Hidden fixtures stay out of the unfiltered listing but remain
		// describable (and creatable) when asked for by name.
		if spec.Hidden && filter == "" {
			continue
		}
		out = append(out, addonSpecToAWS(spec))
	}
	return &eks.DescribeAddonVersionsOutput{Addons: out}, nil
}

// validateAddonServiceAccountRoleArn rejects a non-empty value that is not
// syntactically an IAM role ARN. iam:PassRole at the gateway remains the
// permission and existence check.
func validateAddonServiceAccountRoleArn(roleArn string) error {
	if roleArn == "" {
		return nil
	}
	if err := arn.ValidateRoleARN(roleArn); err != nil {
		return awserrors.Errorf(awserrors.ErrorEKSInvalidParameter,
			"1 validation error detected: Value at 'serviceAccountRoleArn' failed to satisfy constraint: Member must be a valid IAM role ARN")
	}
	return nil
}

// CreateAddon validates, persists a CREATING record, and stages it for delivery.
// Transitions to ACTIVE once the cluster state report confirms delivery.
func (s *EKSServiceImpl) CreateAddon(ctx context.Context, input *eks.CreateAddonInput, accountID string) (*eks.CreateAddonOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	addonName := aws.StringValue(input.AddonName)
	if addonName == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := validateAddonServiceAccountRoleArn(aws.StringValue(input.ServiceAccountRoleArn)); err != nil {
		return nil, err
	}
	spec, ok := addon.Lookup(addonName)
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	version := aws.StringValue(input.AddonVersion)
	if version == "" {
		version = spec.DefaultVersion
	} else if !spec.SupportsVersion(version) {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(ctx, accountID, cluster)
	if err != nil {
		return nil, err
	}
	rec, err := s.addons().Create(ctx, acctKV, accountID, addon.Desired{
		Cluster:               cluster,
		Name:                  addonName,
		Version:               version,
		ServiceAccountRoleArn: aws.StringValue(input.ServiceAccountRoleArn),
		ConfigurationValues:   aws.StringValue(input.ConfigurationValues),
		Tags:                  aws.StringValueMap(input.Tags),
	})
	if err != nil {
		if errors.Is(err, addon.ErrExists) {
			return nil, errors.New(awserrors.ErrorEKSResourceInUse)
		}
		return nil, err
	}
	return &eks.CreateAddonOutput{Addon: addonRecordToAWS(cluster, rec)}, nil
}

// ListStagedAddonManifestsInput names the cluster whose staged add-on manifests
// to return. It is an internal control-plane request (not an AWS-SDK shape),
// served over NATS for the guest addon-sync agent via the internal-addons route.
type ListStagedAddonManifestsInput struct {
	ClusterName string `json:"clusterName"`
}

// ListStagedAddonManifestsOutput carries the staged manifest descriptors, sorted
// by add-on name.
type ListStagedAddonManifestsOutput struct {
	Manifests []eksv1.StagedAddonManifest `json:"manifests"`
}

// ListStagedAddonManifests returns the staged manifest for every add-on staged
// for delivery to a cluster, sorted by add-on name. The guest treats an add-on
// absent here as "remove the locally-rendered manifest".
func (s *EKSServiceImpl) ListStagedAddonManifests(ctx context.Context, input *ListStagedAddonManifestsInput, accountID string) (*ListStagedAddonManifestsOutput, error) {
	if input == nil || input.ClusterName == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(ctx, accountID, input.ClusterName)
	if err != nil {
		return nil, err
	}
	staged, err := addon.ListManifests(ctx, acctKV, input.ClusterName)
	if err != nil {
		return nil, err
	}
	out := make([]eksv1.StagedAddonManifest, 0, len(staged))
	for _, m := range staged {
		out = append(out, eksv1.StagedAddonManifest{
			AddonName:             m.AddonName,
			AddonVersion:          m.AddonVersion,
			ServiceAccountRoleArn: m.ServiceAccountRoleArn,
			ConfigurationValues:   m.ConfigurationValues,
		})
	}
	return &ListStagedAddonManifestsOutput{Manifests: out}, nil
}

// DescribeAddon returns one installed add-on's record.
func (s *EKSServiceImpl) DescribeAddon(ctx context.Context, input *eks.DescribeAddonInput, accountID string) (*eks.DescribeAddonOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	addonName := aws.StringValue(input.AddonName)
	acctKV, err := s.acctKVForCluster(ctx, accountID, cluster)
	if err != nil {
		return nil, err
	}
	rec, err := addon.Get(ctx, acctKV, cluster, addonName)
	if err != nil {
		if errors.Is(err, addon.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.DescribeAddonOutput{Addon: addonRecordToAWS(cluster, rec)}, nil
}

// UpdateAddon CASes new version/config/role onto the record, marks it UPDATING, and re-stages it.
func (s *EKSServiceImpl) UpdateAddon(ctx context.Context, input *eks.UpdateAddonInput, accountID string) (*eks.UpdateAddonOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.ServiceAccountRoleArn != nil {
		if err := validateAddonServiceAccountRoleArn(*input.ServiceAccountRoleArn); err != nil {
			return nil, err
		}
	}
	cluster := aws.StringValue(input.ClusterName)
	addonName := aws.StringValue(input.AddonName)
	acctKV, err := s.acctKVForCluster(ctx, accountID, cluster)
	if err != nil {
		return nil, err
	}
	// Validate a requested version against the catalog before the CAS.
	version := aws.StringValue(input.AddonVersion)
	if version != "" {
		spec, ok := addon.Lookup(addonName)
		if !ok || !spec.SupportsVersion(version) {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	rec, err := s.addons().Update(ctx, acctKV, accountID, cluster, addonName, addon.Change{
		Version:               version,
		ConfigurationValues:   input.ConfigurationValues,
		ServiceAccountRoleArn: input.ServiceAccountRoleArn,
	})
	if err != nil {
		if errors.Is(err, addon.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.UpdateAddonOutput{Update: &eks.Update{
		Id:        aws.String(rec.Arn),
		Status:    aws.String(eks.UpdateStatusSuccessful),
		Type:      aws.String(eks.UpdateTypeAddonUpdate),
		CreatedAt: aws.Time(rec.ModifiedAt),
	}}, nil
}

// DeleteAddon removes the staged manifest and the record, returning the add-on as DELETING.
func (s *EKSServiceImpl) DeleteAddon(ctx context.Context, input *eks.DeleteAddonInput, accountID string) (*eks.DeleteAddonOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	addonName := aws.StringValue(input.AddonName)
	acctKV, err := s.acctKVForCluster(ctx, accountID, cluster)
	if err != nil {
		return nil, err
	}
	rec, err := s.addons().Delete(ctx, acctKV, accountID, cluster, addonName)
	if err != nil {
		if errors.Is(err, addon.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.DeleteAddonOutput{Addon: addonRecordToAWS(cluster, rec)}, nil
}

// addonRecordToAWS converts a persisted record to the SDK Addon shape.
func addonRecordToAWS(cluster string, rec *addon.Record) *eks.Addon {
	out := &eks.Addon{
		AddonArn:     aws.String(rec.Arn),
		AddonName:    aws.String(rec.AddonName),
		AddonVersion: aws.String(rec.AddonVersion),
		ClusterName:  aws.String(cluster),
		Status:       aws.String(string(rec.Status)),
		CreatedAt:    aws.Time(rec.CreatedAt),
		ModifiedAt:   aws.Time(rec.ModifiedAt),
	}
	if rec.ServiceAccountRoleArn != "" {
		out.ServiceAccountRoleArn = aws.String(rec.ServiceAccountRoleArn)
	}
	if rec.ConfigurationValues != "" {
		out.ConfigurationValues = aws.String(rec.ConfigurationValues)
	}
	if rec.Health != "" {
		out.Health = &eks.AddonHealth{Issues: []*eks.AddonIssue{{
			Message: aws.String(rec.Health),
		}}}
	}
	if len(rec.Tags) > 0 {
		out.Tags = aws.StringMap(rec.Tags)
	}
	return out
}

// addonSpecToAWS converts a catalog spec to the SDK AddonInfo shape.
func addonSpecToAWS(spec addon.Spec) *eks.AddonInfo {
	versions := make([]*eks.AddonVersionInfo, 0, len(spec.Versions))
	for _, v := range spec.Versions {
		versions = append(versions, &eks.AddonVersionInfo{
			AddonVersion:           aws.String(v),
			RequiresIamPermissions: aws.Bool(spec.RequiresIRSA),
		})
	}
	return &eks.AddonInfo{
		AddonName:     aws.String(spec.Name),
		AddonVersions: versions,
	}
}

// addons returns the add-on owner, delivering through the injected installer.
func (s *EKSServiceImpl) addons() *addon.Owner {
	return addon.New(s.deps.Region, s.addonInstaller())
}

// addonInstaller returns the injected installer or the default staging installer.
func (s *EKSServiceImpl) addonInstaller() addon.Installer {
	if s.deps.AddonInstaller != nil {
		return s.deps.AddonInstaller
	}
	return addon.NewStagingInstaller(s.addonBucket)
}

// addonBucket opens the account bucket the staging installer writes manifests to.
func (s *EKSServiceImpl) addonBucket(ctx context.Context, accountID string) (jetstream.KeyValue, error) {
	if s.deps.NATSConn == nil {
		return nil, errors.New("eks: stagingInstaller nil NATS connection")
	}
	js, err := jetstream.New(s.deps.NATSConn)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return GetOrCreateAccountBucket(ctx, js, accountID)
}
