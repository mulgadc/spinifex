package handlers_ec2_placementgroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Ensure PlacementGroupServiceImpl implements PlacementGroupService.
var _ PlacementGroupService = (*PlacementGroupServiceImpl)(nil)

const (
	KVBucketPlacementGroups        = "spinifex-placement-groups"
	KVBucketPlacementGroupsVersion = 1
)

// PlacementGroupRecord represents a stored placement group.
type PlacementGroupRecord struct {
	GroupId   string `json:"group_id"`
	GroupName string `json:"group_name"`
	Strategy  string `json:"strategy"`
	State     string `json:"state"`
	// SpreadLevel is always "host" for bare-metal Spinifex clusters.
	SpreadLevel string `json:"spread_level"`
	AccountID   string `json:"account_id"`
	// NodeInstances tracks which node hosts which instances in this group.
	// Key = node name, Value = list of instance IDs on that node.
	NodeInstances map[string][]string `json:"node_instances"`
	Tags          map[string]string   `json:"tags,omitempty"`
}

// PlacementGroupServiceImpl implements placement group operations with NATS JetStream persistence.
type PlacementGroupServiceImpl struct {
	config   *config.Config
	natsConn *nats.Conn
	kv       jetstream.KeyValue
}

// NewPlacementGroupServiceImplWithNATS creates a placement group service with NATS JetStream.
func NewPlacementGroupServiceImplWithNATS(ctx context.Context, cfg *config.Config, natsConn *nats.Conn) (*PlacementGroupServiceImpl, error) {
	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketPlacementGroups, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketPlacementGroups, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketPlacementGroups, kv, KVBucketPlacementGroupsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketPlacementGroups, err)
	}

	slog.Info("Placement group service initialized with JetStream KV", "bucket", KVBucketPlacementGroups)

	return &PlacementGroupServiceImpl{
		config:   cfg,
		natsConn: natsConn,
		kv:       kv,
	}, nil
}

// CreatePlacementGroup creates a new placement group.
func (s *PlacementGroupServiceImpl) CreatePlacementGroup(ctx context.Context, input *ec2.CreatePlacementGroupInput, accountID string) (*ec2.CreatePlacementGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	// AWS treats Strategy as optional, defaulting to cluster.
	strategy := aws.StringValue(input.Strategy)
	if strategy == "" {
		strategy = ec2.PlacementStrategyCluster
	}

	// Only spread and cluster are supported; partition is rejected.
	if strategy == ec2.PlacementStrategyPartition {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if strategy != ec2.PlacementStrategySpread && strategy != ec2.PlacementStrategyCluster {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	groupName := *input.GroupName
	key := utils.AccountKey(accountID, groupName)
	groupID := utils.GenerateResourceID("pg")

	record := PlacementGroupRecord{
		GroupId:       groupID,
		GroupName:     groupName,
		Strategy:      strategy,
		State:         ec2.PlacementGroupStateAvailable,
		SpreadLevel:   ec2.SpreadLevelHost,
		AccountID:     accountID,
		NodeInstances: make(map[string][]string),
		Tags:          utils.ExtractTags(input.TagSpecifications, "placement-group"),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	// Atomic create-if-not-exists to prevent TOCTOU race on duplicate names
	if _, err := s.kv.Create(ctx, key, data); err != nil {
		// Create fails if key already exists
		return nil, errors.New(awserrors.ErrorInvalidPlacementGroupDuplicate)
	}

	slog.InfoContext(ctx, "CreatePlacementGroup completed", "groupId", groupID, "groupName", groupName, "strategy", strategy, "accountID", accountID)

	return &ec2.CreatePlacementGroupOutput{
		PlacementGroup: s.recordToEC2(&record),
	}, nil
}

// DeletePlacementGroup deletes a placement group.
func (s *PlacementGroupServiceImpl) DeletePlacementGroup(ctx context.Context, input *ec2.DeletePlacementGroupInput, accountID string) (*ec2.DeletePlacementGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupName := *input.GroupName
	key := utils.AccountKey(accountID, groupName)

	entry, err := s.kv.Get(ctx, key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
	}

	var record PlacementGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Check for running instances
	instanceCount := 0
	for _, ids := range record.NodeInstances {
		instanceCount += len(ids)
	}
	if instanceCount > 0 {
		return nil, errors.New(awserrors.ErrorInvalidPlacementGroupInUse)
	}

	if err := s.kv.Delete(ctx, key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "DeletePlacementGroup completed", "groupName", groupName, "accountID", accountID)

	return &ec2.DeletePlacementGroupOutput{}, nil
}

var describePlacementGroupsValidFilters = map[string]bool{
	"group-id":     true,
	"strategy":     true,
	"state":        true,
	"spread-level": true,
	"group-name":   true,
	"tag-key":      true,
	"tag-value":    true,
}

// DescribePlacementGroups lists placement groups with optional filters.
func (s *PlacementGroupServiceImpl) DescribePlacementGroups(ctx context.Context, input *ec2.DescribePlacementGroupsInput, accountID string) (*ec2.DescribePlacementGroupsOutput, error) {
	parsedFilters, err := filterutil.ParseFilters(input.Filters, describePlacementGroupsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribePlacementGroups: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Build filter maps for GroupNames/GroupIds parameters
	nameSet := make(map[string]bool)
	for _, name := range input.GroupNames {
		if name != nil {
			nameSet[*name] = true
		}
	}
	idSet := make(map[string]bool)
	for _, id := range input.GroupIds {
		if id != nil {
			idSet[*id] = true
		}
	}

	prefix := accountID + "."
	keys, err := s.kv.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var groups []*ec2.PlacementGroup
	for _, k := range keys {
		if k == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(k, prefix) {
			continue
		}

		entry, err := s.kv.Get(ctx, k)
		if err != nil {
			slog.WarnContext(ctx, "Failed to get placement group record", "key", k, "error", err)
			continue
		}

		var record PlacementGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.WarnContext(ctx, "Failed to unmarshal placement group record", "key", k, "error", err)
			continue
		}

		// Apply name filter (from GroupNames parameter)
		if len(nameSet) > 0 && !nameSet[record.GroupName] {
			continue
		}
		// Apply ID filter (from GroupIds parameter)
		if len(idSet) > 0 && !idSet[record.GroupId] {
			continue
		}
		if !pgMatchesFilters(&record, parsedFilters) {
			continue
		}

		groups = append(groups, s.recordToEC2(&record))
	}

	// If specific names were requested but not found, return error
	if len(nameSet) > 0 {
		found := make(map[string]bool)
		for _, g := range groups {
			if g.GroupName != nil {
				found[*g.GroupName] = true
			}
		}
		for name := range nameSet {
			if !found[name] {
				return nil, errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
			}
		}
	}

	slog.InfoContext(ctx, "DescribePlacementGroups completed", "count", len(groups), "accountID", accountID)

	return &ec2.DescribePlacementGroupsOutput{
		PlacementGroups: groups,
	}, nil
}

// pgMatchesFilters checks whether a placement group record matches all parsed filters.
func pgMatchesFilters(record *PlacementGroupRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}
		switch name {
		case "group-id":
			if !filterutil.MatchesAny(values, record.GroupId) {
				return false
			}
		case "strategy":
			if !filterutil.MatchesAny(values, record.Strategy) {
				return false
			}
		case "state":
			if !filterutil.MatchesAny(values, record.State) {
				return false
			}
		case "spread-level":
			if !filterutil.MatchesAny(values, record.SpreadLevel) {
				return false
			}
		case "group-name":
			if !filterutil.MatchesAny(values, record.GroupName) {
				return false
			}
		case "tag-key":
			if !pgMatchesAnyTag(record.Tags, values, func(k, _ string) string { return k }) {
				return false
			}
		case "tag-value":
			if !pgMatchesAnyTag(record.Tags, values, func(_, v string) string { return v }) {
				return false
			}
		default:
			return false
		}
	}
	return filterutil.MatchesTags(filters, record.Tags)
}

// pgMatchesAnyTag reports whether any tag's selected field (key or value)
// matches any of the filter values.
func pgMatchesAnyTag(tags map[string]string, values []string, field func(k, v string) string) bool {
	for k, v := range tags {
		if filterutil.MatchesAny(values, field(k, v)) {
			return true
		}
	}
	return false
}

const maxCASRetries = 5

// errPGAbsent and errPGContended mark the two kvutil outcomes that map to
// specific AWS errors rather than a generic internal failure. Neither
// escapes this file.
var (
	errPGAbsent    = errors.New("placementgroup: record absent")
	errPGContended = errors.New("placementgroup: CAS retries exhausted")
)

// mapCASError turns a kvutil failure into the AWS error the caller returns.
// A non-kvutil error came from mutate and is already an AWS code.
func mapCASError(ctx context.Context, op, groupName, accountID string, err error) error {
	switch {
	case errors.Is(err, errPGAbsent):
		return errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
	case errors.Is(err, errPGContended):
		slog.ErrorContext(ctx, "Placement group CAS retries exhausted under contention",
			"op", op, "groupName", groupName, "accountID", accountID)
	case errors.Is(err, kvutil.ErrRead):
		slog.ErrorContext(ctx, "Failed to read placement group from KV",
			"op", op, "groupName", groupName, "accountID", accountID, "err", err)
	case errors.Is(err, kvutil.ErrDecode):
		slog.ErrorContext(ctx, "Corrupt placement group record in KV",
			"op", op, "groupName", groupName, "accountID", accountID, "err", err)
	case errors.Is(err, kvutil.ErrEncode):
		slog.ErrorContext(ctx, "Failed to marshal placement group record",
			"op", op, "groupName", groupName, "accountID", accountID, "err", err)
	case errors.Is(err, kvutil.ErrWrite):
		slog.ErrorContext(ctx, "Failed to write placement group to KV",
			"op", op, "groupName", groupName, "accountID", accountID, "err", err)
	default:
		return err
	}
	return errors.New(awserrors.ErrorServerInternal)
}

// casConfig is the shared CAS policy for every placement group mutation.
func casConfig() kvutil.CASConfig {
	return kvutil.CASConfig{
		Attempts:  maxCASRetries,
		NotFound:  errPGAbsent,
		Exhausted: func(string, int) error { return errPGContended },
	}
}

// ReserveSpreadNodes atomically reserves node slots for a spread placement group launch.
// Filters occupied nodes, selects up to MaxCount, writes placeholders via CAS with retries.
func (s *PlacementGroupServiceImpl) ReserveSpreadNodes(ctx context.Context, input *ReserveSpreadNodesInput, accountID string) (*ReserveSpreadNodesOutput, error) {
	if input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	var selected []string
	_, err := kvutil.Update(ctx, s.kv, utils.AccountKey(accountID, input.GroupName), casConfig(),
		func(record *PlacementGroupRecord) (bool, error) {
			if record.State != ec2.PlacementGroupStateAvailable {
				return false, errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
			}
			if record.Strategy != ec2.PlacementStrategySpread {
				return false, errors.New(awserrors.ErrorInvalidParameterValue)
			}

			// Eligible means it has capacity and hosts nothing from this group yet.
			var available []string
			for _, node := range input.EligibleNodes {
				if _, occupied := record.NodeInstances[node]; !occupied {
					available = append(available, node)
				}
			}
			if len(available) < input.MinCount {
				return false, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
			}

			// An empty instance list reserves the node without claiming a launch.
			selected = available[:min(input.MaxCount, len(available))]
			for _, node := range selected {
				record.NodeInstances[node] = []string{}
			}
			return true, nil
		})

	if err != nil {
		return nil, mapCASError(ctx, "ReserveSpreadNodes", input.GroupName, accountID, err)
	}

	slog.InfoContext(ctx, "ReserveSpreadNodes completed", "groupName", input.GroupName, "nodes", selected, "accountID", accountID)
	return &ReserveSpreadNodesOutput{ReservedNodes: selected}, nil
}

// FinalizeSpreadInstances replaces placeholder entries with actual instance IDs.
// Uses CAS with retries following the IPAM pattern.
func (s *PlacementGroupServiceImpl) FinalizeSpreadInstances(ctx context.Context, input *FinalizeSpreadInstancesInput, accountID string) (*FinalizeSpreadInstancesOutput, error) {
	if input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	_, err := kvutil.Update(ctx, s.kv, utils.AccountKey(accountID, input.GroupName), casConfig(),
		func(record *PlacementGroupRecord) (bool, error) {
			maps.Copy(record.NodeInstances, input.NodeInstances)
			return true, nil
		})
	if err != nil {
		return nil, mapCASError(ctx, "FinalizeSpreadInstances", input.GroupName, accountID, err)
	}

	slog.InfoContext(ctx, "FinalizeSpreadInstances completed", "groupName", input.GroupName, "accountID", accountID)
	return &FinalizeSpreadInstancesOutput{}, nil
}

// ReleaseSpreadNodes removes placeholder entries for nodes that failed to launch.
// Uses CAS with retries following the IPAM pattern.
func (s *PlacementGroupServiceImpl) ReleaseSpreadNodes(ctx context.Context, input *ReleaseSpreadNodesInput, accountID string) (*ReleaseSpreadNodesOutput, error) {
	if input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	releaseSet := make(map[string]bool, len(input.Nodes))
	for _, n := range input.Nodes {
		releaseSet[n] = true
	}

	_, err := kvutil.Update(ctx, s.kv, utils.AccountKey(accountID, input.GroupName), casConfig(),
		func(record *PlacementGroupRecord) (bool, error) {
			changed := false
			for node := range releaseSet {
				// Only placeholders are releasable. A node holding instance IDs is in use and must not be droppped.
				if ids, ok := record.NodeInstances[node]; ok && len(ids) == 0 {
					delete(record.NodeInstances, node)
					changed = true
				}
			}
			return changed, nil
		})
	if err != nil {
		return nil, mapCASError(ctx, "ReleaseSpreadNodes", input.GroupName, accountID, err)
	}

	slog.InfoContext(ctx, "ReleaseSpreadNodes completed", "groupName", input.GroupName, "nodes", input.Nodes, "accountID", accountID)
	return &ReleaseSpreadNodesOutput{}, nil
}

// RemoveInstance removes a specific instance from a placement group's NodeInstances.
// If the node's instance list becomes empty after removal, the node key is deleted.
// Uses CAS with retries following the IPAM pattern.
func (s *PlacementGroupServiceImpl) RemoveInstance(ctx context.Context, input *RemoveInstanceInput, accountID string) (*RemoveInstanceOutput, error) {
	if input.GroupName == "" || input.InstanceID == "" || input.NodeName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	_, err := kvutil.Update(ctx, s.kv, utils.AccountKey(accountID, input.GroupName), casConfig(),
		func(record *PlacementGroupRecord) (bool, error) {
			instances, exists := record.NodeInstances[input.NodeName]
			if !exists {
				return false, nil // node not tracked, nothing to remove
			}

			filtered := make([]string, 0, len(instances))
			for _, id := range instances {
				if id != input.InstanceID {
					filtered = append(filtered, id)
				}
			}
			// A node with no instances left stops occupying a spread slot.
			if len(filtered) == 0 {
				delete(record.NodeInstances, input.NodeName)
			} else {
				record.NodeInstances[input.NodeName] = filtered
			}
			return true, nil
		})
	if errors.Is(err, errPGAbsent) {
		// Group already deleted, so there is nothing left to remove.
		slog.DebugContext(ctx, "RemoveInstance: group not found, treating as success", "groupName", input.GroupName)
		return &RemoveInstanceOutput{}, nil
	}
	if err != nil {
		return nil, mapCASError(ctx, "RemoveInstance", input.GroupName, accountID, err)
	}

	slog.InfoContext(ctx, "RemoveInstance completed", "groupName", input.GroupName, "instanceId", input.InstanceID, "node", input.NodeName, "accountID", accountID)
	return &RemoveInstanceOutput{}, nil
}

// ReserveClusterNode determines the target node for a cluster placement group launch.
// If the group already has instances, it returns the existing node (cluster = all on one node).
// If empty, it picks the first eligible node (highest capacity) and writes a placeholder via CAS.
func (s *PlacementGroupServiceImpl) ReserveClusterNode(ctx context.Context, input *ReserveClusterNodeInput, accountID string) (*ReserveClusterNodeOutput, error) {
	if input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	var targetNode string
	_, err := kvutil.Update(ctx, s.kv, utils.AccountKey(accountID, input.GroupName), casConfig(),
		func(record *PlacementGroupRecord) (bool, error) {
			if record.State != ec2.PlacementGroupStateAvailable {
				return false, errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
			}
			if record.Strategy != ec2.PlacementStrategyCluster {
				return false, errors.New(awserrors.ErrorInvalidParameterValue)
			}

			// A cluster group keeps every instance on one node, so an existing node is the answer and nothing needs writing.
			for node := range record.NodeInstances {
				targetNode = node
				return false, nil
			}

			if len(input.EligibleNodes) == 0 {
				return false, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
			}
			targetNode = input.EligibleNodes[0] // sorted desc by capacity by the caller
			record.NodeInstances[targetNode] = []string{}
			return true, nil
		})
	if err != nil {
		return nil, mapCASError(ctx, "ReserveClusterNode", input.GroupName, accountID, err)
	}

	slog.InfoContext(ctx, "ReserveClusterNode completed", "groupName", input.GroupName, "targetNode", targetNode, "accountID", accountID)
	return &ReserveClusterNodeOutput{TargetNode: targetNode}, nil
}

// FinalizeClusterInstances appends launched instance IDs to the cluster placement group record.
// Uses CAS with retries following the IPAM pattern.
func (s *PlacementGroupServiceImpl) FinalizeClusterInstances(ctx context.Context, input *FinalizeClusterInstancesInput, accountID string) (*FinalizeClusterInstancesOutput, error) {
	if input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	_, err := kvutil.Update(ctx, s.kv, utils.AccountKey(accountID, input.GroupName), casConfig(),
		func(record *PlacementGroupRecord) (bool, error) {
			// Append rather than replace: a cluster group can have several launches in flight at once.
			for node, ids := range input.NodeInstances {
				record.NodeInstances[node] = append(record.NodeInstances[node], ids...)
			}
			return true, nil
		})
	if err != nil {
		return nil, mapCASError(ctx, "FinalizeClusterInstances", input.GroupName, accountID, err)
	}

	slog.InfoContext(ctx, "FinalizeClusterInstances completed", "groupName", input.GroupName, "accountID", accountID)
	return &FinalizeClusterInstancesOutput{}, nil
}

// recordToEC2 converts an internal record to the AWS SDK PlacementGroup type.
func (s *PlacementGroupServiceImpl) recordToEC2(record *PlacementGroupRecord) *ec2.PlacementGroup {
	pg := &ec2.PlacementGroup{
		GroupId:   aws.String(record.GroupId),
		GroupName: aws.String(record.GroupName),
		Strategy:  aws.String(record.Strategy),
		State:     aws.String(record.State),
		Tags:      utils.MapToEC2Tags(record.Tags),
	}
	if record.Strategy == ec2.PlacementStrategySpread {
		pg.SpreadLevel = aws.String(record.SpreadLevel)
	}
	return pg
}
