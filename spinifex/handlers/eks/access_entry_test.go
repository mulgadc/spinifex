package handlers_eks

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAccessScope(t *testing.T) {
	cases := []struct {
		name    string
		scope   *eks.AccessScope
		want    AccessScope
		wantErr bool
	}{
		{"nil scope", nil, AccessScope{}, true},
		{"empty type", &eks.AccessScope{}, AccessScope{}, true},
		{"cluster ok", &eks.AccessScope{Type: aws.String("cluster")}, AccessScope{Type: "cluster"}, false},
		{"cluster mixed case", &eks.AccessScope{Type: aws.String("Cluster")}, AccessScope{Type: "cluster"}, false},
		{"cluster with namespaces", &eks.AccessScope{Type: aws.String("cluster"), Namespaces: aws.StringSlice([]string{"kube-system"})}, AccessScope{}, true},
		{"namespace ok", &eks.AccessScope{Type: aws.String("namespace"), Namespaces: aws.StringSlice([]string{"team-a", "team-b"})}, AccessScope{Type: "namespace", Namespaces: []string{"team-a", "team-b"}}, false},
		{"namespace without namespaces", &eks.AccessScope{Type: aws.String("namespace")}, AccessScope{}, true},
		{"unsupported type", &eks.AccessScope{Type: aws.String("galaxy")}, AccessScope{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateAccessScope(tc.scope)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAccessEntryRecordToAWS_IncludesTags(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	rec := &AccessEntryRecord{
		ARN:                "arn:aws:eks:ap-southeast-2:000000000001:access-entry/dev1/abc",
		ClusterName:        "dev1",
		PrincipalARN:       "arn:aws:iam::000000000001:user/admin",
		KubernetesUsername: "arn:aws:iam::000000000001:user/admin",
		KubernetesGroups:   []string{"system:masters"},
		Type:               AccessEntryTypeStandard,
		Tags:               map[string]string{"team": "platform"},
		CreatedAt:          now,
		ModifiedAt:         now,
	}
	out := accessEntryRecordToAWS(rec)
	assert.Equal(t, "dev1", aws.StringValue(out.ClusterName))
	assert.Equal(t, rec.PrincipalARN, aws.StringValue(out.PrincipalArn))
	assert.Equal(t, []string{"system:masters"}, aws.StringValueSlice(out.KubernetesGroups))
	require.NotNil(t, out.Tags)
	assert.Equal(t, "platform", aws.StringValue(out.Tags["team"]))
}

func TestAssociatedPolicyToAWS_ScopeNamespaces(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	p := AssociatedAccessPolicy{
		PolicyARN:    "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy",
		AccessScope:  AccessScope{Type: "namespace", Namespaces: []string{"team-a"}},
		AssociatedAt: now,
		ModifiedAt:   now,
	}
	out := associatedPolicyToAWS(p)
	assert.Equal(t, p.PolicyARN, aws.StringValue(out.PolicyArn))
	assert.Equal(t, "namespace", aws.StringValue(out.AccessScope.Type))
	assert.Equal(t, []string{"team-a"}, aws.StringValueSlice(out.AccessScope.Namespaces))

	cluster := associatedPolicyToAWS(AssociatedAccessPolicy{
		PolicyARN:   "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy",
		AccessScope: AccessScope{Type: "cluster"},
	})
	assert.Equal(t, "cluster", aws.StringValue(cluster.AccessScope.Type))
	assert.Empty(t, cluster.AccessScope.Namespaces)
}

// The record-store guard clauses reject malformed input before touching the KV,
// so a nil handle is enough to exercise them.
func TestAccessEntryRecordGuards(t *testing.T) {
	require.Error(t, PutAccessEntryRecord(nil, nil))
	require.Error(t, PutAccessEntryRecord(nil, &AccessEntryRecord{PrincipalARN: "arn:aws:iam::000000000001:user/admin"}))

	_, err := GetAccessEntryRecord(nil, "", "arn:aws:iam::000000000001:user/admin")
	require.Error(t, err)

	_, err = ListAccessEntryRecords(nil, "")
	require.Error(t, err)
}
