package handlers_eks

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/nats-io/nats.go"
)

// Access-entry types. v1 supports STANDARD principals only; node types
// (EC2_LINUX/EC2_WINDOWS/FARGATE_LINUX) are rejected until the nodegroup
// join path wires them.
const (
	AccessEntryTypeStandard = "STANDARD"

	accessScopeCluster   = "cluster"
	accessScopeNamespace = "namespace"
)

// supportedAccessPolicies maps each supported AWS-managed policy ARN to the
// K8s ClusterRole it grants. Policy ARNs outside this set are rejected.
var supportedAccessPolicies = map[string]string{
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy": "cluster-admin",
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy":        "admin",
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy":         "edit",
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy":         "view",
}

// ErrAccessEntryNotFound is returned when no entry exists for the principal.
// Callers translate it to ResourceNotFoundException at the service boundary.
var ErrAccessEntryNotFound = errors.New("eks: access entry not found")

// AccessEntryARN composes the access-entry ARN, keying the discriminator off
// the principal-ARN hash for determinism. Clients address entries by
// principalArn, not this ARN, so the divergence from AWS's UUID is informational.
func AccessEntryARN(region, accountID, cluster, principalARN string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:access-entry/%s/%s",
		region, accountID, cluster, PrincipalARNHash(principalARN))
}

// PutAccessEntryRecord writes the entry unconditionally.
func PutAccessEntryRecord(kv nats.KeyValue, rec *AccessEntryRecord) error {
	if rec == nil {
		return errors.New("eks: PutAccessEntryRecord nil record")
	}
	if rec.ClusterName == "" || rec.PrincipalARN == "" {
		return errors.New("eks: PutAccessEntryRecord missing cluster or principal ARN")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal access entry %s: %w", rec.PrincipalARN, err)
	}
	key := AccessEntryKey(rec.ClusterName, rec.PrincipalARN)
	if _, err := kv.Put(key, data); err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

// GetAccessEntryRecord reads one entry. Returns ErrAccessEntryNotFound if absent.
func GetAccessEntryRecord(kv nats.KeyValue, cluster, principalARN string) (*AccessEntryRecord, error) {
	if cluster == "" || principalARN == "" {
		return nil, errors.New("eks: GetAccessEntryRecord empty cluster or principal ARN")
	}
	entry, err := kv.Get(AccessEntryKey(cluster, principalARN))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, ErrAccessEntryNotFound
		}
		return nil, fmt.Errorf("kv get access entry: %w", err)
	}
	var rec AccessEntryRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal access entry %s: %w", principalARN, err)
	}
	return &rec, nil
}

// ListAccessEntryRecords returns all access entries under a cluster, sorted by principal ARN.
func ListAccessEntryRecords(kv nats.KeyValue, cluster string) ([]*AccessEntryRecord, error) {
	if cluster == "" {
		return nil, errors.New("eks: ListAccessEntryRecords empty cluster")
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("kv keys: %w", err)
	}
	prefix := AccessEntriesPrefix(cluster)
	out := make([]*AccessEntryRecord, 0)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := kv.Get(k)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("kv get %s: %w", k, err)
		}
		var rec AccessEntryRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("unmarshal access entry %s: %w", k, err)
		}
		out = append(out, &rec)
	}
	sortAccessEntries(out)
	return out, nil
}

// DeleteAccessEntryRecord removes one entry; returns ErrAccessEntryNotFound if absent.
func DeleteAccessEntryRecord(kv nats.KeyValue, cluster, principalARN string) error {
	key := AccessEntryKey(cluster, principalARN)
	if _, err := kv.Get(key); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return ErrAccessEntryNotFound
		}
		return fmt.Errorf("kv get %s: %w", key, err)
	}
	if err := kv.Delete(key); err != nil {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	return nil
}

// casUpdateAccessEntry does a revision-checked read-modify-write.
// mutate returns true when a field changed. Returns ErrAccessEntryNotFound if absent.
func casUpdateAccessEntry(kv nats.KeyValue, cluster, principalARN string, mutate func(*AccessEntryRecord) bool) (*AccessEntryRecord, error) {
	key := AccessEntryKey(cluster, principalARN)
	for range maxClusterStateCASRetries {
		entry, err := kv.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				return nil, ErrAccessEntryNotFound
			}
			return nil, fmt.Errorf("kv get %s: %w", key, err)
		}
		var rec AccessEntryRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("unmarshal access entry %s: %w", principalARN, err)
		}
		if !mutate(&rec) {
			return &rec, nil
		}
		data, err := json.Marshal(&rec)
		if err != nil {
			return nil, fmt.Errorf("marshal access entry %s: %w", principalARN, err)
		}
		_, err = kv.Update(key, data, entry.Revision())
		if err == nil {
			return &rec, nil
		}
		if errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		return nil, fmt.Errorf("kv update %s: %w", key, err)
	}
	return nil, fmt.Errorf("eks: casUpdateAccessEntry %s exhausted CAS retries", principalARN)
}

// sortAccessEntries orders entries by principal ARN. One record exists per
// principal, so the ordering is total and an unstable sort suffices.
func sortAccessEntries(recs []*AccessEntryRecord) {
	slices.SortFunc(recs, func(a, b *AccessEntryRecord) int {
		return strings.Compare(a.PrincipalARN, b.PrincipalARN)
	})
}

// validateAccessScope validates an AccessScope: type must be "cluster" or
// "namespace"; namespace scope requires ≥1 namespace, cluster scope must have none.
func validateAccessScope(scope *eks.AccessScope) (AccessScope, error) {
	if scope == nil || aws.StringValue(scope.Type) == "" {
		return AccessScope{}, errors.New("eks: accessScope.type is required")
	}
	t := strings.ToLower(aws.StringValue(scope.Type))
	ns := aws.StringValueSlice(scope.Namespaces)
	switch t {
	case accessScopeCluster:
		if len(ns) > 0 {
			return AccessScope{}, errors.New("eks: cluster-scoped policy must not list namespaces")
		}
		return AccessScope{Type: accessScopeCluster}, nil
	case accessScopeNamespace:
		if len(ns) == 0 {
			return AccessScope{}, errors.New("eks: namespace-scoped policy requires at least one namespace")
		}
		return AccessScope{Type: accessScopeNamespace, Namespaces: ns}, nil
	default:
		return AccessScope{}, fmt.Errorf("eks: unsupported accessScope.type %q", t)
	}
}

// accessEntryRecordToAWS converts the persisted record to the SDK shape.
func accessEntryRecordToAWS(rec *AccessEntryRecord) *eks.AccessEntry {
	out := &eks.AccessEntry{
		AccessEntryArn:   aws.String(rec.ARN),
		ClusterName:      aws.String(rec.ClusterName),
		PrincipalArn:     aws.String(rec.PrincipalARN),
		Username:         aws.String(rec.KubernetesUsername),
		KubernetesGroups: aws.StringSlice(rec.KubernetesGroups),
		Type:             aws.String(rec.Type),
		CreatedAt:        aws.Time(rec.CreatedAt),
		ModifiedAt:       aws.Time(rec.ModifiedAt),
	}
	if len(rec.Tags) > 0 {
		out.Tags = aws.StringMap(rec.Tags)
	}
	return out
}

// associatedPolicyToAWS converts a persisted associated policy to the SDK shape.
func associatedPolicyToAWS(p AssociatedAccessPolicy) *eks.AssociatedAccessPolicy {
	scope := &eks.AccessScope{Type: aws.String(p.AccessScope.Type)}
	if len(p.AccessScope.Namespaces) > 0 {
		scope.Namespaces = aws.StringSlice(p.AccessScope.Namespaces)
	}
	return &eks.AssociatedAccessPolicy{
		PolicyArn:    aws.String(p.PolicyARN),
		AccessScope:  scope,
		AssociatedAt: aws.Time(p.AssociatedAt),
		ModifiedAt:   aws.Time(p.ModifiedAt),
	}
}

// newAccessEntryRecord builds a record; defaults username to principalARN when unset.
func newAccessEntryRecord(region, accountID, cluster, principalARN, username string, groups []string, entryType string, tags map[string]string, now time.Time) *AccessEntryRecord {
	// AWS defaults the Kubernetes username to the principal ARN when omitted.
	if username == "" {
		username = principalARN
	}
	return &AccessEntryRecord{
		ARN:                AccessEntryARN(region, accountID, cluster, principalARN),
		ClusterName:        cluster,
		PrincipalARN:       principalARN,
		KubernetesUsername: username,
		KubernetesGroups:   groups,
		Type:               entryType,
		Tags:               tags,
		CreatedAt:          now,
		ModifiedAt:         now,
	}
}
