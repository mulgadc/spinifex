package handlers_eks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonObjectKeys returns the top-level field names of a decoded JSON object,
// for asserting the exact set of keys a struct marshaled (omitempty and all).
func jsonObjectKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// Reads the raw persisted bytes back with an independently computed key
// (sha256 hex of the principal ARN) rather than via PrincipalARNHash, so the
// test does not just re-check the helper against itself.
func TestAccessEntryPersistedLayout_TopLevelAndNestedFieldNames(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")

	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName:      aws.String("c1"),
		PrincipalArn:     aws.String(testPrincipalARN),
		KubernetesGroups: aws.StringSlice([]string{"g1"}),
		Tags:             aws.StringMap(map[string]string{"env": "prod"}),
	}, testAccountID)
	require.NoError(t, err)

	const viewPolicy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
	_, err = svc.AssociateAccessPolicy(context.Background(), &eks.AssociateAccessPolicyInput{
		ClusterName:  aws.String("c1"),
		PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:    aws.String(viewPolicy),
		AccessScope:  &eks.AccessScope{Type: aws.String("cluster")},
	}, testAccountID)
	require.NoError(t, err)

	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID)
	require.NoError(t, err)

	sum := sha256.Sum256([]byte(testPrincipalARN))
	key := fmt.Sprintf("clusters/c1/access-entries/%s", hex.EncodeToString(sum[:]))
	entry, err := kv.Get(t.Context(), key)
	require.NoError(t, err)

	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(entry.Value(), &top))
	assert.ElementsMatch(t, []string{
		"arn", "clusterName", "principalARN", "kubernetesUsername", "kubernetesGroups",
		"type", "tags", "associatedPolicies", "createdAt", "modifiedAt",
	}, jsonObjectKeys(top))

	var policies []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(top["associatedPolicies"], &policies))
	require.Len(t, policies, 1)
	assert.ElementsMatch(t, []string{"policyARN", "accessScope", "associatedAt", "modifiedAt"},
		jsonObjectKeys(policies[0]))

	var scope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(policies[0]["accessScope"], &scope))
	assert.ElementsMatch(t, []string{"type"}, jsonObjectKeys(scope))
}

// A hand-written blob using exactly the field names above must decode into
// AccessEntryRecord with the expected values, pinning the wire shape
// independently of whatever the marshaling code currently does.
func TestAccessEntryRecord_DecodesHandWrittenJSON(t *testing.T) {
	t.Parallel()
	blob := `{
		"arn": "arn:aws:eks:us-east-1:111122223333:access-entry/c1/abc123",
		"clusterName": "c1",
		"principalARN": "arn:aws:iam::111122223333:role/dev",
		"kubernetesUsername": "dev-user",
		"kubernetesGroups": ["g1", "g2"],
		"type": "STANDARD",
		"tags": {"env": "prod"},
		"associatedPolicies": [
			{
				"policyARN": "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy",
				"accessScope": {"type": "cluster"},
				"associatedAt": "2024-01-01T00:00:00Z",
				"modifiedAt": "2024-01-02T00:00:00Z"
			}
		],
		"createdAt": "2024-01-01T00:00:00Z",
		"modifiedAt": "2024-01-02T00:00:00Z"
	}`

	var rec AccessEntryRecord
	require.NoError(t, json.Unmarshal([]byte(blob), &rec))

	assert.Equal(t, "arn:aws:eks:us-east-1:111122223333:access-entry/c1/abc123", rec.ARN)
	assert.Equal(t, "c1", rec.ClusterName)
	assert.Equal(t, "arn:aws:iam::111122223333:role/dev", rec.PrincipalARN)
	assert.Equal(t, "dev-user", rec.KubernetesUsername)
	assert.Equal(t, []string{"g1", "g2"}, rec.KubernetesGroups)
	assert.Equal(t, AccessEntryTypeStandard, rec.Type)
	assert.Equal(t, map[string]string{"env": "prod"}, rec.Tags)
	require.Len(t, rec.AssociatedPolicies, 1)
	assert.Equal(t, "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", rec.AssociatedPolicies[0].PolicyARN)
	assert.Equal(t, AccessScope{Type: "cluster"}, rec.AssociatedPolicies[0].AccessScope)
	assert.Equal(t, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), rec.AssociatedPolicies[0].AssociatedAt)
	assert.Equal(t, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), rec.AssociatedPolicies[0].ModifiedAt)
	assert.Equal(t, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), rec.CreatedAt)
	assert.Equal(t, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), rec.ModifiedAt)
}

// Pins present behavior: acctKVForCluster only checks the cluster exists, not
// its lifecycle Status, so AccessEntry actions succeed on a DELETING or FAILED
// cluster exactly as on an ACTIVE one.
func TestAccessEntryActionsIgnoreClusterLifecycleStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cluster string
		status  ClusterStatus
	}{
		{"deleting-cluster", ClusterStatusDeleting},
		{"failed-cluster", ClusterStatusFailed},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			svc := setupTestService(t)
			js := testutil.NewJetStream(t, svc.deps.NATSConn)
			kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID)
			require.NoError(t, err)
			require.NoError(t, PutClusterMeta(t.Context(), kv, &ClusterMeta{Name: tc.cluster, Status: tc.status}))

			out, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
				ClusterName: aws.String(tc.cluster), PrincipalArn: aws.String(testPrincipalARN),
			}, testAccountID)
			require.NoError(t, err)
			require.NotNil(t, out.AccessEntry)

			desc, err := svc.DescribeAccessEntry(context.Background(), &eks.DescribeAccessEntryInput{
				ClusterName: aws.String(tc.cluster), PrincipalArn: aws.String(testPrincipalARN),
			}, testAccountID)
			require.NoError(t, err)
			require.NotNil(t, desc.AccessEntry)
		})
	}
}

// AssociateAccessPolicy on a principal with no AccessEntry today returns the
// same ResourceNotFoundException code as an unknown cluster, via
// casUpdateAccessEntry surfacing ErrAccessEntryNotFound.
func TestAccessPolicy_AssociateWithNoEntryIsNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")

	_, err := svc.AssociateAccessPolicy(context.Background(), &eks.AssociateAccessPolicyInput{
		ClusterName:  aws.String("c1"),
		PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:    aws.String("arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"),
		AccessScope:  &eks.AccessScope{Type: aws.String("cluster")},
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

// Disassociating a policy that was never associated succeeds today (the
// mutate callback returns false, so casUpdateAccessEntry treats it as a
// no-op) and leaves the whole record, including ModifiedAt, untouched.
func TestAccessPolicy_DisassociateUnassociatedLeavesRecordUnchanged(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)

	const viewPolicy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
	const editPolicy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy"
	_, err = svc.AssociateAccessPolicy(context.Background(), &eks.AssociateAccessPolicyInput{
		ClusterName:  aws.String("c1"),
		PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:    aws.String(viewPolicy),
		AccessScope:  &eks.AccessScope{Type: aws.String("cluster")},
	}, testAccountID)
	require.NoError(t, err)

	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID)
	require.NoError(t, err)
	before, err := GetAccessEntryRecord(t.Context(), kv, "c1", testPrincipalARN)
	require.NoError(t, err)

	_, err = svc.DisassociateAccessPolicy(context.Background(), &eks.DisassociateAccessPolicyInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN), PolicyArn: aws.String(editPolicy),
	}, testAccountID)
	require.NoError(t, err)

	after, err := GetAccessEntryRecord(t.Context(), kv, "c1", testPrincipalARN)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// UpdateAccessEntry with neither Username nor KubernetesGroups set keeps both
// stored values, but the mutate callback always returns true unconditionally,
// so ModifiedAt still advances even on this no-op update.
func TestAccessEntry_UpdateWithNoFieldsKeepsUsernameAndGroups(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName:      aws.String("c1"),
		PrincipalArn:     aws.String(testPrincipalARN),
		Username:         aws.String("alice"),
		KubernetesGroups: aws.StringSlice([]string{"g1"}),
	}, testAccountID)
	require.NoError(t, err)

	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID)
	require.NoError(t, err)
	before, err := GetAccessEntryRecord(t.Context(), kv, "c1", testPrincipalARN)
	require.NoError(t, err)

	time.Sleep(time.Millisecond)
	out, err := svc.UpdateAccessEntry(context.Background(), &eks.UpdateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "alice", aws.StringValue(out.AccessEntry.Username))
	assert.Equal(t, []string{"g1"}, aws.StringValueSlice(out.AccessEntry.KubernetesGroups))

	after, err := GetAccessEntryRecord(t.Context(), kv, "c1", testPrincipalARN)
	require.NoError(t, err)
	assert.Equal(t, "alice", after.KubernetesUsername)
	assert.Equal(t, []string{"g1"}, after.KubernetesGroups)
	assert.True(t, after.ModifiedAt.After(before.ModifiedAt))
}

// DeleteClusterPrefix_SweepsEveryKey (cluster_state_test.go) covers the
// nodegroup/OIDC/event keys; this covers the access-entries key specifically,
// scoped correctly so a sibling cluster's entry survives.
func TestDeleteClusterPrefix_SweepsAccessEntriesScopedToCluster(t *testing.T) {
	t.Parallel()
	kv := newClusterStateTestKV(t)

	now := time.Now().UTC()
	require.NoError(t, PutAccessEntryRecord(t.Context(), kv, &AccessEntryRecord{
		ClusterName:        "cluster-a",
		PrincipalARN:       testPrincipalARN,
		KubernetesUsername: testPrincipalARN,
		Type:               AccessEntryTypeStandard,
		CreatedAt:          now,
		ModifiedAt:         now,
	}))
	require.NoError(t, PutAccessEntryRecord(t.Context(), kv, &AccessEntryRecord{
		ClusterName:        "cluster-b",
		PrincipalARN:       testPrincipalARN,
		KubernetesUsername: testPrincipalARN,
		Type:               AccessEntryTypeStandard,
		CreatedAt:          now,
		ModifiedAt:         now,
	}))

	require.NoError(t, DeleteClusterPrefix(t.Context(), kv, "cluster-a"))

	_, err := GetAccessEntryRecord(t.Context(), kv, "cluster-a", testPrincipalARN)
	require.ErrorIs(t, err, ErrAccessEntryNotFound)

	survivor, err := GetAccessEntryRecord(t.Context(), kv, "cluster-b", testPrincipalARN)
	require.NoError(t, err)
	assert.Equal(t, "cluster-b", survivor.ClusterName)
}

// Pins present behavior: ResolveTokenReview opens the account KV directly and
// never reads ClusterMeta, so an AccessEntry resolves identity even when no
// cluster meta record exists at all for the named cluster.
func TestResolveTokenReview_AuthenticatesWithoutClusterMeta(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	kv := seedAccountBucket(t, js, testAccountID)
	require.NoError(t, PutAccessEntryRecord(t.Context(), kv, &AccessEntryRecord{
		ClusterName:        "alpha",
		PrincipalARN:       testARN,
		KubernetesUsername: testARN,
		KubernetesGroups:   []string{"system:masters"},
		Type:               AccessEntryTypeStandard,
	}))

	_, err := GetClusterMeta(t.Context(), kv, "alpha")
	require.ErrorIs(t, err, ErrClusterNotFound)

	sub, err := nc.Subscribe(TokenVerifySubject, func(m *nats.Msg) {
		resp, _ := json.Marshal(TokenVerifyResponse{
			AccountID: testAccountID, ARN: testARN, UserID: "AROAEXAMPLE:session",
		})
		_ = m.Respond(resp)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	res, err := ResolveTokenReview(context.Background(), nc, testAccountID, "alpha", validToken("https://sts/?x=1"), 2*time.Second)
	require.NoError(t, err)
	require.True(t, res.Authenticated)
	assert.Equal(t, []string{"system:masters"}, res.Groups)
}
