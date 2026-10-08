package handlers_eks

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/access"
)

// validateAccessScope validates an AccessScope: type must be "cluster" or
// "namespace"; namespace scope requires ≥1 namespace, cluster scope must have none.
func validateAccessScope(scope *eks.AccessScope) (access.Scope, error) {
	if scope == nil || aws.StringValue(scope.Type) == "" {
		return access.Scope{}, errors.New("eks: accessScope.type is required")
	}
	t := strings.ToLower(aws.StringValue(scope.Type))
	ns := aws.StringValueSlice(scope.Namespaces)
	switch t {
	case access.ScopeCluster:
		if len(ns) > 0 {
			return access.Scope{}, errors.New("eks: cluster-scoped policy must not list namespaces")
		}
		return access.Scope{Type: access.ScopeCluster}, nil
	case access.ScopeNamespace:
		if len(ns) == 0 {
			return access.Scope{}, errors.New("eks: namespace-scoped policy requires at least one namespace")
		}
		return access.Scope{Type: access.ScopeNamespace, Namespaces: ns}, nil
	default:
		return access.Scope{}, fmt.Errorf("eks: unsupported accessScope.type %q", t)
	}
}

// accessEntryRecordToAWS converts the persisted record to the SDK shape.
func accessEntryRecordToAWS(rec *access.Record) *eks.AccessEntry {
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
func associatedPolicyToAWS(p access.AssociatedPolicy) *eks.AssociatedAccessPolicy {
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
