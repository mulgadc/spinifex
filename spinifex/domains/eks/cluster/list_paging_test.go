package cluster

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/access"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/addon"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listPage calls one list action at a given token and returns its items and next token.
type listPage func(token *string) ([]string, *string, error)

// walkPages follows nextToken from the first page to the last and returns
// every page.
func walkPages(t *testing.T, call listPage) [][]string {
	t.Helper()
	var pages [][]string
	var token *string
	for range 10 {
		items, next, err := call(token)
		require.NoError(t, err)
		pages = append(pages, items)
		if next == nil {
			return pages
		}
		require.NotEmpty(t, *next)
		token = next
	}
	t.Fatal("listing did not end")
	return nil
}

func pagingService(t *testing.T) (*EKSServiceImpl, jetstream.KeyValue) {
	t.Helper()
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	kv, err := GetOrCreateAccountBucket(t.Context(), testutil.NewJetStream(t, svc.deps.NATSConn), testAccountID)
	require.NoError(t, err)
	return svc, kv
}

func TestListNodegroups_PagesByName(t *testing.T) {
	t.Parallel()
	svc, kv := pagingService(t)
	for _, n := range []string{"ng-c", "ng-a", "ng-b"} {
		require.NoError(t, PutNodegroupRecord(t.Context(), kv, &NodegroupRecord{ClusterName: "c1", Name: n}))
	}

	pages := walkPages(t, func(token *string) ([]string, *string, error) {
		out, err := svc.ListNodegroups(context.Background(), &eks.ListNodegroupsInput{
			ClusterName: aws.String("c1"), MaxResults: aws.Int64(2), NextToken: token,
		}, testAccountID)
		if err != nil {
			return nil, nil, err
		}
		return aws.StringValueSlice(out.Nodegroups), out.NextToken, nil
	})
	assert.Equal(t, [][]string{{"ng-a", "ng-b"}, {"ng-c"}}, pages)
}

func TestListAddons_PagesByName(t *testing.T) {
	t.Parallel()
	svc, kv := pagingService(t)
	for _, n := range []string{"addon-c", "addon-a", "addon-b"} {
		data, err := json.Marshal(&addon.Record{AddonName: n})
		require.NoError(t, err)
		_, err = kv.Put(t.Context(), addon.Key("c1", n), data)
		require.NoError(t, err)
	}

	pages := walkPages(t, func(token *string) ([]string, *string, error) {
		out, err := svc.ListAddons(context.Background(), &eks.ListAddonsInput{
			ClusterName: aws.String("c1"), MaxResults: aws.Int64(2), NextToken: token,
		}, testAccountID)
		if err != nil {
			return nil, nil, err
		}
		return aws.StringValueSlice(out.Addons), out.NextToken, nil
	})
	assert.Equal(t, [][]string{{"addon-a", "addon-b"}, {"addon-c"}}, pages)
}

func TestListAccessEntries_PagesByPrincipal(t *testing.T) {
	t.Parallel()
	svc, _ := pagingService(t)
	principals := []string{
		"arn:aws:iam::111122223333:role/c",
		"arn:aws:iam::111122223333:role/a",
		"arn:aws:iam::111122223333:role/b",
	}
	for _, p := range principals {
		_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
			ClusterName: aws.String("c1"), PrincipalArn: aws.String(p),
		}, testAccountID)
		require.NoError(t, err)
	}

	pages := walkPages(t, func(token *string) ([]string, *string, error) {
		out, err := svc.ListAccessEntries(context.Background(), &eks.ListAccessEntriesInput{
			ClusterName: aws.String("c1"), MaxResults: aws.Int64(2), NextToken: token,
		}, testAccountID)
		if err != nil {
			return nil, nil, err
		}
		return aws.StringValueSlice(out.AccessEntries), out.NextToken, nil
	})
	assert.Equal(t, [][]string{{principals[1], principals[2]}, {principals[0]}}, pages)
}

func TestListAssociatedAccessPolicies_PagesByPolicyArn(t *testing.T) {
	t.Parallel()
	svc, _ := pagingService(t)
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)
	const (
		admin = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy"
		edit  = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy"
		view  = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
	)
	for _, p := range []string{view, admin, edit} {
		require.Contains(t, access.SupportedPolicies, p)
		_, err := svc.AssociateAccessPolicy(context.Background(), &eks.AssociateAccessPolicyInput{
			ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
			PolicyArn: aws.String(p), AccessScope: &eks.AccessScope{Type: aws.String("cluster")},
		}, testAccountID)
		require.NoError(t, err)
	}

	pages := walkPages(t, func(token *string) ([]string, *string, error) {
		out, err := svc.ListAssociatedAccessPolicies(context.Background(), &eks.ListAssociatedAccessPoliciesInput{
			ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN), MaxResults: aws.Int64(2), NextToken: token,
		}, testAccountID)
		if err != nil {
			return nil, nil, err
		}
		arns := make([]string, 0, len(out.AssociatedAccessPolicies))
		for _, p := range out.AssociatedAccessPolicies {
			arns = append(arns, aws.StringValue(p.PolicyArn))
		}
		return arns, out.NextToken, nil
	})
	assert.Equal(t, [][]string{{admin, edit}, {view}}, pages)
}

func TestListAccessPolicies_PagesByArn(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	pages := walkPages(t, func(token *string) ([]string, *string, error) {
		out, err := svc.ListAccessPolicies(context.Background(), &eks.ListAccessPoliciesInput{
			MaxResults: aws.Int64(3), NextToken: token,
		}, testAccountID)
		if err != nil {
			return nil, nil, err
		}
		arns := make([]string, 0, len(out.AccessPolicies))
		for _, p := range out.AccessPolicies {
			arns = append(arns, aws.StringValue(p.Arn))
		}
		return arns, out.NextToken, nil
	})
	require.Len(t, pages, 2)
	assert.Len(t, pages[0], 3)
	assert.Equal(t, len(access.SupportedPolicies), len(pages[0])+len(pages[1]))
	assert.Less(t, pages[0][2], pages[1][0], "pages must follow ARN order")
}

// A token is opaque: a bare item key is refused rather than resumed from.
func TestListAccessPolicies_RejectsForeignToken(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	_, err := svc.ListAccessPolicies(context.Background(), &eks.ListAccessPoliciesInput{
		NextToken: aws.String("arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorEKSInvalidParameter, awserrors.ValidErrorCodeFromError(err))
}

// Every EKS list action refuses a maxResults outside the model's 1 to 100
// range instead of clamping it to a page.
func TestListActions_RefuseOutOfRangeMaxResults(t *testing.T) {
	t.Parallel()
	svc, _ := pagingService(t)
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)
	ctx := context.Background()
	cluster, principal := aws.String("c1"), aws.String(testPrincipalARN)
	calls := map[string]func(mr *int64) error{
		"ListClusters": func(mr *int64) error {
			_, err := svc.ListClusters(ctx, &eks.ListClustersInput{MaxResults: mr}, testAccountID)
			return err
		},
		"ListClustersNoAccountBucket": func(mr *int64) error {
			_, err := svc.ListClusters(ctx, &eks.ListClustersInput{MaxResults: mr}, "999999999999")
			return err
		},
		"ListNodegroups": func(mr *int64) error {
			_, err := svc.ListNodegroups(ctx, &eks.ListNodegroupsInput{ClusterName: cluster, MaxResults: mr}, testAccountID)
			return err
		},
		"ListAddons": func(mr *int64) error {
			_, err := svc.ListAddons(ctx, &eks.ListAddonsInput{ClusterName: cluster, MaxResults: mr}, testAccountID)
			return err
		},
		"ListAccessEntries": func(mr *int64) error {
			_, err := svc.ListAccessEntries(ctx, &eks.ListAccessEntriesInput{ClusterName: cluster, MaxResults: mr}, testAccountID)
			return err
		},
		"ListAccessPolicies": func(mr *int64) error {
			_, err := svc.ListAccessPolicies(ctx, &eks.ListAccessPoliciesInput{MaxResults: mr}, testAccountID)
			return err
		},
		"ListAssociatedAccessPolicies": func(mr *int64) error {
			_, err := svc.ListAssociatedAccessPolicies(ctx, &eks.ListAssociatedAccessPoliciesInput{
				ClusterName: cluster, PrincipalArn: principal, MaxResults: mr,
			}, testAccountID)
			return err
		},
		"DescribeAddonVersions": func(mr *int64) error {
			_, err := svc.DescribeAddonVersions(ctx, &eks.DescribeAddonVersionsInput{MaxResults: mr}, testAccountID)
			return err
		},
	}
	for name, call := range calls {
		for _, mr := range []int64{0, -1, 101} {
			err := call(aws.Int64(mr))
			require.Error(t, err, "%s maxResults=%d", name, mr)
			assert.Equal(t, awserrors.ErrorEKSInvalidParameter, awserrors.ValidErrorCodeFromError(err), "%s maxResults=%d", name, mr)
		}
		for _, mr := range []int64{1, 100} {
			require.NoError(t, call(aws.Int64(mr)), "%s maxResults=%d", name, mr)
		}
	}
}
