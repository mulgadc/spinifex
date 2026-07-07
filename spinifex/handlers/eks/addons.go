package handlers_eks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// ListAddons returns the names of every managed add-on installed on a cluster.
func (s *EKSServiceImpl) ListAddons(ctx context.Context, input *eks.ListAddonsInput, accountID string) (*eks.ListAddonsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	recs, err := ListAddonRecords(acctKV, cluster)
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
	specs := catalogSpecs()
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
	spec, ok := lookupAddon(addonName)
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	version := aws.StringValue(input.AddonVersion)
	if version == "" {
		version = spec.DefaultVersion
	} else if !spec.supportsVersion(version) {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	if _, err := GetAddonRecord(acctKV, cluster, addonName); err == nil {
		return nil, errors.New(awserrors.ErrorEKSResourceInUse)
	} else if !errors.Is(err, ErrAddonNotFound) {
		return nil, err
	}
	now := time.Now().UTC()
	rec := &AddonRecord{
		AddonName:             addonName,
		AddonVersion:          version,
		Status:                AddonStatusCreating,
		ServiceAccountRoleArn: aws.StringValue(input.ServiceAccountRoleArn),
		ConfigurationValues:   aws.StringValue(input.ConfigurationValues),
		Arn:                   AddonARN(s.deps.Region, accountID, cluster, addonName),
		Tags:                  aws.StringValueMap(input.Tags),
		CreatedAt:             now,
		ModifiedAt:            now,
	}
	if err := PutAddonRecord(acctKV, cluster, rec); err != nil {
		return nil, err
	}
	if err := s.addonInstaller().Install(accountID, cluster, rec); err != nil {
		s.markAddonFailed(acctKV, cluster, addonName, err)
		return nil, err
	}
	return &eks.CreateAddonOutput{Addon: addonRecordToAWS(cluster, rec)}, nil
}

// ListStagedAddonManifestsInput names the cluster whose staged add-on manifests
// to return. It is an internal control-plane request (not an AWS-SDK shape),
// served over NATS for the on-VM addon-sync agent via the internal-addons
// gateway route.
type ListStagedAddonManifestsInput struct {
	ClusterName string `json:"clusterName"`
}

// ListStagedAddonManifestsOutput carries the staged manifest descriptors, sorted
// by add-on name.
type ListStagedAddonManifestsOutput struct {
	Manifests []StagedAddonManifest `json:"manifests"`
}

// ListStagedAddonManifests returns the staged manifest descriptor for every
// add-on currently staged for delivery to a cluster, sorted by add-on name.
// The on-VM addon-sync agent fetches these (via the internal-addons gateway
// route) to render the baked bundles into the K3s auto-deploy dir; an add-on
// whose record was deleted has its staged manifest removed, so the agent treats
// absence here as "remove the locally-rendered manifest".
func (s *EKSServiceImpl) ListStagedAddonManifests(ctx context.Context, input *ListStagedAddonManifestsInput, accountID string) (*ListStagedAddonManifestsOutput, error) {
	if input == nil || input.ClusterName == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(accountID, input.ClusterName)
	if err != nil {
		return nil, err
	}
	keys, err := acctKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return &ListStagedAddonManifestsOutput{Manifests: []StagedAddonManifest{}}, nil
		}
		return nil, err
	}
	prefix := AddonsPrefix(input.ClusterName)
	out := make([]StagedAddonManifest, 0)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, "/manifest") {
			continue
		}
		entry, err := acctKV.Get(k)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return nil, err
		}
		var m StagedAddonManifest
		if err := json.Unmarshal(entry.Value(), &m); err != nil {
			return nil, fmt.Errorf("unmarshal staged manifest %s: %w", k, err)
		}
		out = append(out, m)
	}
	sortStagedManifests(out)
	return &ListStagedAddonManifestsOutput{Manifests: out}, nil
}

func sortStagedManifests(m []StagedAddonManifest) {
	for i := 1; i < len(m); i++ {
		for j := i; j > 0 && m[j-1].AddonName > m[j].AddonName; j-- {
			m[j-1], m[j] = m[j], m[j-1]
		}
	}
}

// DescribeAddon returns one installed add-on's record.
func (s *EKSServiceImpl) DescribeAddon(ctx context.Context, input *eks.DescribeAddonInput, accountID string) (*eks.DescribeAddonOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	addonName := aws.StringValue(input.AddonName)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	rec, err := GetAddonRecord(acctKV, cluster, addonName)
	if err != nil {
		if errors.Is(err, ErrAddonNotFound) {
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
	cluster := aws.StringValue(input.ClusterName)
	addonName := aws.StringValue(input.AddonName)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	// Validate a requested version against the catalog before the CAS.
	if v := aws.StringValue(input.AddonVersion); v != "" {
		spec, ok := lookupAddon(addonName)
		if !ok || !spec.supportsVersion(v) {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	now := time.Now().UTC()
	rec, err := casUpdateAddon(acctKV, cluster, addonName, func(r *AddonRecord) bool {
		if v := aws.StringValue(input.AddonVersion); v != "" {
			r.AddonVersion = v
		}
		if input.ConfigurationValues != nil {
			r.ConfigurationValues = aws.StringValue(input.ConfigurationValues)
		}
		if input.ServiceAccountRoleArn != nil {
			r.ServiceAccountRoleArn = aws.StringValue(input.ServiceAccountRoleArn)
		}
		r.Status = AddonStatusUpdating
		r.ModifiedAt = now
		return true
	})
	if err != nil {
		if errors.Is(err, ErrAddonNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	if err := s.addonInstaller().Install(accountID, cluster, rec); err != nil {
		s.markAddonFailed(acctKV, cluster, addonName, err)
		return nil, err
	}
	return &eks.UpdateAddonOutput{Update: &eks.Update{
		Id:        aws.String(rec.Arn),
		Status:    aws.String(eks.UpdateStatusSuccessful),
		Type:      aws.String(eks.UpdateTypeAddonUpdate),
		CreatedAt: aws.Time(now),
	}}, nil
}

// DeleteAddon marks the record DELETING, removes the staged manifest, then deletes the record.
func (s *EKSServiceImpl) DeleteAddon(ctx context.Context, input *eks.DeleteAddonInput, accountID string) (*eks.DeleteAddonOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	addonName := aws.StringValue(input.AddonName)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	rec, err := GetAddonRecord(acctKV, cluster, addonName)
	if err != nil {
		if errors.Is(err, ErrAddonNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	rec.Status = AddonStatusDeleting
	rec.ModifiedAt = time.Now().UTC()
	out := &eks.DeleteAddonOutput{Addon: addonRecordToAWS(cluster, rec)}
	if err := s.addonInstaller().Uninstall(accountID, cluster, addonName); err != nil {
		return nil, err
	}
	if err := DeleteAddonRecord(acctKV, cluster, addonName); err != nil {
		if errors.Is(err, ErrAddonNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return out, nil
}

// addonRecordToAWS converts a persisted record to the SDK Addon shape.
func addonRecordToAWS(cluster string, rec *AddonRecord) *eks.Addon {
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
func addonSpecToAWS(spec AddonSpec) *eks.AddonInfo {
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

// addonInstaller returns the injected installer or the default stagingInstaller.
func (s *EKSServiceImpl) addonInstaller() AddonInstaller {
	if s.deps.AddonInstaller != nil {
		return s.deps.AddonInstaller
	}
	return newStagingInstaller(s.deps.NATSConn)
}

// markAddonFailed best-effort flips a record to CREATE_FAILED with the error reason.
func (s *EKSServiceImpl) markAddonFailed(acctKV nats.KeyValue, cluster, addon string, cause error) {
	now := time.Now().UTC()
	if _, err := casUpdateAddon(acctKV, cluster, addon, func(r *AddonRecord) bool {
		r.Status = AddonStatusCreateFailed
		r.Health = cause.Error()
		r.ModifiedAt = now
		return true
	}); err != nil {
		slog.Warn("markAddonFailed: CAS failed", "cluster", cluster, "addon", addon, "err", err)
	}
}
