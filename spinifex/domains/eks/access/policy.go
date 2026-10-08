package access

import (
	"log/slog"
	"slices"
)

// SupportedPolicies maps each supported AWS-managed policy ARN to the K8s
// ClusterRole it grants. Policy ARNs outside this set are rejected.
var SupportedPolicies = map[string]string{
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy": "cluster-admin",
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy":        "admin",
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy":         "edit",
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy":         "view",
}

// policyGroups maps each supported policy ARN to the Kubernetes group its
// cluster-scope association projects: system:masters for the cluster-admin
// policy, a synthetic mulga: group bound by a static manifest for the rest.
var policyGroups = map[string]string{
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy": "system:masters",
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy":        "mulga:eks-admin",
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy":         "mulga:eks-edit",
	"arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy":         "mulga:eks-view",
}

// EffectiveGroups returns rec's static groups plus the Kubernetes group each
// cluster-scoped associated policy projects, deduplicated in first-seen order.
// A namespace-scoped or unrecognized association contributes nothing, so an
// older or wider record degrades safely instead of widening scope or panicking.
func EffectiveGroups(rec *Record) []string {
	seen := make(map[string]struct{}, len(rec.KubernetesGroups))
	groups := make([]string, 0, len(rec.KubernetesGroups))
	add := func(g string) {
		if g == "" {
			return
		}
		if _, ok := seen[g]; ok {
			return
		}
		seen[g] = struct{}{}
		groups = append(groups, g)
	}
	for _, g := range rec.KubernetesGroups {
		add(g)
	}
	for _, p := range rec.AssociatedPolicies {
		if p.AccessScope.Type != ScopeCluster {
			continue
		}
		g, ok := policyGroups[p.PolicyARN]
		if !ok {
			slog.Debug("access policy projection: unrecognized policy ARN", "policy_arn", p.PolicyARN)
			continue
		}
		add(g)
	}
	return groups
}

// HasAssociatedPolicy reports whether the entry has the given policy ARN bound.
func HasAssociatedPolicy(rec *Record, policyARN string) bool {
	return slices.ContainsFunc(rec.AssociatedPolicies, func(p AssociatedPolicy) bool {
		return p.PolicyARN == policyARN
	})
}
