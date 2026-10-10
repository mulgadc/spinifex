package access

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEffectiveGroups_EachPolicyProducesItsGroup(t *testing.T) {
	cases := []struct {
		policyARN string
		want      string
	}{
		{"arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy", "system:masters"},
		{"arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy", "mulga:eks-admin"},
		{"arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy", "mulga:eks-edit"},
		{"arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", "mulga:eks-view"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			rec := &Record{
				AssociatedPolicies: []AssociatedPolicy{
					{PolicyARN: tc.policyARN, AccessScope: Scope{Type: ScopeCluster}},
				},
			}
			assert.Equal(t, []string{tc.want}, EffectiveGroups(rec))
		})
	}
}

func TestEffectiveGroups_TwoAssociationsProduceBothGroups(t *testing.T) {
	t.Parallel()
	rec := &Record{
		AssociatedPolicies: []AssociatedPolicy{
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy", AccessScope: Scope{Type: ScopeCluster}},
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", AccessScope: Scope{Type: ScopeCluster}},
		},
	}
	assert.Equal(t, []string{"mulga:eks-edit", "mulga:eks-view"}, EffectiveGroups(rec))
}

// A creator seeded system:masters directly (CreateCluster bootstrap) who is
// also associated to AmazonEKSClusterAdminPolicy must not see the group twice.
func TestEffectiveGroups_DedupesSeededAndAssociatedClusterAdmin(t *testing.T) {
	t.Parallel()
	rec := &Record{
		KubernetesGroups: []string{"system:masters"},
		AssociatedPolicies: []AssociatedPolicy{
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy", AccessScope: Scope{Type: ScopeCluster}},
		},
	}
	assert.Equal(t, []string{"system:masters"}, EffectiveGroups(rec))
}

// Records written before namespace scope was rejected at association time may
// still carry one; it must not silently widen to a cluster-scope grant.
func TestEffectiveGroups_NamespaceScopeContributesNothing(t *testing.T) {
	t.Parallel()
	rec := &Record{
		AssociatedPolicies: []AssociatedPolicy{
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", AccessScope: Scope{Type: ScopeNamespace, Namespaces: []string{"team-a"}}},
		},
	}
	assert.Empty(t, EffectiveGroups(rec))
}

func TestEffectiveGroups_UnrecognizedPolicyDoesNotPanic(t *testing.T) {
	t.Parallel()
	rec := &Record{
		AssociatedPolicies: []AssociatedPolicy{
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/MadeUpPolicy", AccessScope: Scope{Type: ScopeCluster}},
		},
	}
	assert.NotPanics(t, func() { EffectiveGroups(rec) })
	assert.Empty(t, EffectiveGroups(rec))
}
