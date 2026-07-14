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
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure PlacementGroupServiceImpl implements PlacementGroupService
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
	kv       nats.KeyValue
}

// NewPlacementGroupServiceImplWithNATS creates a placement group service with NATS JetStream.
func NewPlacementGroupServiceImplWithNATS(cfg *config.Config, natsConn *nats.Conn) (*PlacementGroupServiceImpl, error) {
	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := utils.GetOrCreateKVBucket(js, KVBucketPlacementGroups, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketPlacementGroups, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketPlacementGroups, kv, KVBucketPlacementGroupsVersion); err != nil {
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
	if _, err := s.kv.Create(key, data); err != nil {
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

	entry, err := s.kv.Get(key)
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

	if err := s.kv.Delete(key); err != nil {
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
	keys, err := s.kv.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
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

		entry, err := s.kv.Get(k)
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

// GetPlacementGroupRecord reads a placement group record from KV with its revision for CAS operations.
// Returns the record and the KV entry (for revision). Exported for use by gateway spread routing.
func (s *PlacementGroupServiceImpl) GetPlacementGroupRecord(accountID, groupName string) (*PlacementGroupRecord, nats.KeyValueEntry, error) {
	key := utils.AccountKey(accountID, groupName)
	entry, err := s.kv.Get(key)
	if err != nil {
		return nil, nil, errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
	}

	var record PlacementGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, nil, errors.New(awserrors.ErrorServerInternal)
	}

	return &record, entry, nil
}

// UpdatePlacementGroupRecord writes a placement group record using CAS (optimistic concurrency).
// Returns nil on success or the error on CAS conflict.
func (s *PlacementGroupServiceImpl) UpdatePlacementGroupRecord(accountID, groupName string, record *PlacementGroupRecord, revision uint64) error {
	key := utils.AccountKey(accountID, groupName)
	data, err := json.Marshal(record)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.kv.Update(key, data, revision); err != nil {
		return err
	}
	return nil
}

const maxCASRetries = 5

// ReserveSpreadNodes atomically reserves node slots for a spread placement group launch.
// Filters occupied nodes, selects up to MaxCount, writes placeholders via CAS with retries.
func (s *PlacementGroupServiceImpl) ReserveSpreadNodes(ctx context.Context, input *ReserveSpreadNodesInput, accountID string) (*ReserveSpreadNodesOutput, error) {
	if input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	for attempt := range maxCASRetries {
		record, entry, err := s.GetPlacementGroupRecord(accountID, input.GroupName)
		if err != nil {
			return nil, err
		}

		if record.State != ec2.PlacementGroupStateAvailable {
			return nil, errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
		}
		if record.Strategy != ec2.PlacementStrategySpread {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}

		// Build set of nodes already hosting instances in this group
		occupiedNodes := make(map[string]bool)
		for node := range record.NodeInstances {
			occupiedNodes[node] = true
		}

		// Filter eligible nodes: must have capacity AND not already occupied
		var available []string
		for _, node := range input.EligibleNodes {
			if !occupiedNodes[node] {
				available = append(available, node)
			}
		}

		if len(available) < input.MinCount {
			return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
		}

		// Select nodes: up to MaxCount, at least MinCount
		launchCount := min(input.MaxCount, len(available))
		selected := available[:launchCount]

		// Add placeholder entries (empty instance list = reserved but not yet launched)
		for _, node := range selected {
			record.NodeInstances[node] = []string{}
		}

		// CAS write — retry on conflict
		if err := s.UpdatePlacementGroupRecord(accountID, input.GroupName, record, entry.Revision()); err != nil {
			slog.DebugContext(ctx, "ReserveSpreadNodes: CAS conflict, retrying", "attempt", attempt, "err", err)
			continue
		}

		slog.InfoContext(ctx, "ReserveSpreadNodes completed", "groupName", input.GroupName, "nodes", selected, "accountID", accountID)
		return &ReserveSpreadNodesOutput{ReservedNodes: selected}, nil
	}

	slog.ErrorContext(ctx, "ReserveSpreadNodes: CAS retries exhausted", "groupName", input.GroupName, "accountID", accountID)
	return nil, errors.New(awserrors.ErrorServerInternal)
}

// FinalizeSpreadInstances replaces placeholder entries with actual instance IDs.
// Uses CAS with retries following the IPAM pattern.
func (s *PlacementGroupServiceImpl) FinalizeSpreadInstances(ctx context.Context, input *FinalizeSpreadInstancesInput, accountID string) (*FinalizeSpreadInstancesOutput, error) {
	if input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	for attempt := range maxCASRetries {
		record, entry, err := s.GetPlacementGroupRecord(accountID, input.GroupName)
		if err != nil {
			return nil, err
		}

		// Replace placeholder entries with actual instance IDs per node
		maps.Copy(record.NodeInstances, input.NodeInstances)

		if err := s.UpdatePlacementGroupRecord(accountID, input.GroupName, record, entry.Revision()); err != nil {
			slog.DebugContext(ctx, "FinalizeSpreadInstances: CAS conflict, retrying", "attempt", attempt, "err", err)
			continue
		}

		slog.InfoContext(ctx, "FinalizeSpreadInstances completed", "groupName", input.GroupName, "accountID", accountID)
		return &FinalizeSpreadInstancesOutput{}, nil
	}

	slog.ErrorContext(ctx, "FinalizeSpreadInstances: CAS retries exhausted", "groupName", input.GroupName, "accountID", accountID)
	return nil, errors.New(awserrors.ErrorServerInternal)
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

	for attempt := range maxCASRetries {
		record, entry, err := s.GetPlacementGroupRecord(accountID, input.GroupName)
		if err != nil {
			return nil, err
		}

		for node := range releaseSet {
			// Only release placeholder entries (empty instance list).
			// Entries with real instance IDs must not be deleted.
			if ids, ok := record.NodeInstances[node]; ok && len(ids) == 0 {
				delete(record.NodeInstances, node)
			}
		}

		if err := s.UpdatePlacementGroupRecord(accountID, input.GroupName, record, entry.Revision()); err != nil {
			slog.DebugContext(ctx, "ReleaseSpreadNodes: CAS conflict, retrying", "attempt", attempt, "err", err)
			continue
		}

		slog.InfoContext(ctx, "ReleaseSpreadNodes completed", "groupName", input.GroupName, "nodes", input.Nodes, "accountID", accountID)
		return &ReleaseSpreadNodesOutput{}, nil
	}

	slog.ErrorContext(ctx, "ReleaseSpreadNodes: CAS retries exhausted", "groupName", input.GroupName, "accountID", accountID)
	return nil, errors.New(awserrors.ErrorServerInternal)
}

// RemoveInstance removes a specific instance from a placement group's NodeInstances.
// If the node's instance list becomes empty after removal, the node key is deleted.
// Uses CAS with retries following the IPAM pattern.
func (s *PlacementGroupServiceImpl) RemoveInstance(ctx context.Context, input *RemoveInstanceInput, accountID string) (*RemoveInstanceOutput, error) {
	if input.GroupName == "" || input.InstanceID == "" || input.NodeName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	for attempt := range maxCASRetries {
		record, entry, lookupErr := s.GetPlacementGroupRecord(accountID, input.GroupName)
		if lookupErr != nil {
			// Group may have been deleted already — treat as success
			slog.DebugContext(ctx, "RemoveInstance: group not found, treating as success", "groupName", input.GroupName)
			return &RemoveInstanceOutput{}, nil //nolint:nilerr // intentional: deleted group = success
		}

		instances, exists := record.NodeInstances[input.NodeName]
		if !exists {
			// Node not tracked — nothing to remove
			return &RemoveInstanceOutput{}, nil
		}

		// Remove the specific instance ID
		filtered := make([]string, 0, len(instances))
		for _, id := range instances {
			if id != input.InstanceID {
				filtered = append(filtered, id)
			}
		}

		if len(filtered) == 0 {
			delete(record.NodeInstances, input.NodeName)
		} else {
			record.NodeInstances[input.NodeName] = filtered
		}

		if err := s.UpdatePlacementGroupRecord(accountID, input.GroupName, record, entry.Revision()); err != nil {
			slog.DebugContext(ctx, "RemoveInstance: CAS conflict, retrying", "attempt", attempt, "err", err)
			continue
		}

		slog.InfoContext(ctx, "RemoveInstance completed", "groupName", input.GroupName, "instanceId", input.InstanceID, "node", input.NodeName, "accountID", accountID)
		return &RemoveInstanceOutput{}, nil
	}

	slog.ErrorContext(ctx, "RemoveInstance: CAS retries exhausted", "groupName", input.GroupName, "instanceId", input.InstanceID, "accountID", accountID)
	return nil, errors.New(awserrors.ErrorServerInternal)
}

// ReserveClusterNode determines the target node for a cluster placement group launch.
// If the group already has instances, it returns the existing node (cluster = all on one node).
// If empty, it picks the first eligible node (highest capacity) and writes a placeholder via CAS.
func (s *PlacementGroupServiceImpl) ReserveClusterNode(ctx context.Context, input *ReserveClusterNodeInput, accountID string) (*ReserveClusterNodeOutput, error) {
	if input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	for attempt := range maxCASRetries {
		record, entry, err := s.GetPlacementGroupRecord(accountID, input.GroupName)
		if err != nil {
			return nil, err
		}
		if record.State != ec2.PlacementGroupStateAvailable {
			return nil, errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
		}
		if record.Strategy != ec2.PlacementStrategyCluster {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}

		// If existing node, return it immediately (no CAS write needed)
		for node := range record.NodeInstances {
			return &ReserveClusterNodeOutput{TargetNode: node}, nil
		}

		// Empty group: need to pick and claim a node via CAS
		if len(input.EligibleNodes) == 0 {
			return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
		}

		targetNode := input.EligibleNodes[0] // first = highest capacity (sorted desc by caller)
		record.NodeInstances[targetNode] = []string{}

		if err := s.UpdatePlacementGroupRecord(accountID, input.GroupName, record, entry.Revision()); err != nil {
			slog.DebugContext(ctx, "ReserveClusterNode: CAS conflict, retrying", "attempt", attempt, "err", err)
			continue
		}

		slog.InfoContext(ctx, "ReserveClusterNode completed", "groupName", input.GroupName, "targetNode", targetNode, "accountID", accountID)
		return &ReserveClusterNodeOutput{TargetNode: targetNode}, nil
	}

	slog.ErrorContext(ctx, "ReserveClusterNode: CAS retries exhausted", "groupName", input.GroupName, "accountID", accountID)
	return nil, errors.New(awserrors.ErrorServerInternal)
}

// FinalizeClusterInstances appends launched instance IDs to the cluster placement group record.
// Uses CAS with retries following the IPAM pattern.
func (s *PlacementGroupServiceImpl) FinalizeClusterInstances(ctx context.Context, input *FinalizeClusterInstancesInput, accountID string) (*FinalizeClusterInstancesOutput, error) {
	if input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	for attempt := range maxCASRetries {
		record, entry, err := s.GetPlacementGroupRecord(accountID, input.GroupName)
		if err != nil {
			return nil, err
		}

		// Append new instance IDs to existing entries (cluster may have concurrent launches)
		for node, ids := range input.NodeInstances {
			record.NodeInstances[node] = append(record.NodeInstances[node], ids...)
		}

		if err := s.UpdatePlacementGroupRecord(accountID, input.GroupName, record, entry.Revision()); err != nil {
			slog.DebugContext(ctx, "FinalizeClusterInstances: CAS conflict, retrying", "attempt", attempt, "err", err)
			continue
		}

		slog.InfoContext(ctx, "FinalizeClusterInstances completed", "groupName", input.GroupName, "accountID", accountID)
		return &FinalizeClusterInstancesOutput{}, nil
	}

	slog.ErrorContext(ctx, "FinalizeClusterInstances: CAS retries exhausted", "groupName", input.GroupName, "accountID", accountID)
	return nil, errors.New(awserrors.ErrorServerInternal)
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
