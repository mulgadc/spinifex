package gateway_eks_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/arn"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	authzRegion    = "ap-southeast-2"
	authzAccountID = "123456789012"
)

func resolveARNs(t *testing.T, action string, params []string, body string) []string {
	t.Helper()
	resources, err := gateway_eks.ResourceARNs(action, authzRegion, authzAccountID, params, []byte(body))
	require.NoError(t, err)
	return resources
}

// The nodegroup ARN the create path persists for the same identifiers. Spelled
// through the shared builders so the test cannot pass by agreeing with itself.
func nodegroupARN(cluster, name string) string {
	return arn.FormatEKSNodegroup(authzRegion, authzAccountID, cluster, name,
		arn.EKSNodegroupDiscriminator(authzAccountID, cluster, name))
}

// One assertion per resource class: the ARN handed to the evaluator must name
// the object the handler acts on, not a plausible-looking neighbour.
func TestResourceARNsFidelity(t *testing.T) {
	tests := []struct {
		action string
		params []string
		body   string
		want   []string
	}{
		{
			action: "DescribeCluster",
			params: []string{"prod"},
			want:   []string{"arn:aws:eks:ap-southeast-2:123456789012:cluster/prod"},
		},
		{
			action: "CreateCluster",
			body:   `{"name":"prod","version":"1.31"}`,
			want:   []string{"arn:aws:eks:ap-southeast-2:123456789012:cluster/prod"},
		},
		{
			action: "DeleteNodegroup",
			params: []string{"prod", "workers"},
			want:   []string{nodegroupARN("prod", "workers")},
		},
		{
			action: "CreateNodegroup",
			params: []string{"prod"},
			body:   `{"nodegroupName":"workers"}`,
			want: []string{
				"arn:aws:eks:ap-southeast-2:123456789012:cluster/prod",
				nodegroupARN("prod", "workers"),
			},
		},
		{
			action: "DescribeAddon",
			params: []string{"prod", "coredns"},
			want:   []string{"arn:aws:eks:ap-southeast-2:123456789012:addon/prod/coredns"},
		},
		{
			action: "CreateAddon",
			params: []string{"prod"},
			body:   `{"addonName":"coredns"}`,
			want: []string{
				"arn:aws:eks:ap-southeast-2:123456789012:cluster/prod",
				"arn:aws:eks:ap-southeast-2:123456789012:addon/prod/coredns",
			},
		},
		{
			action: "ListAddons",
			params: []string{"prod"},
			want:   []string{"arn:aws:eks:ap-southeast-2:123456789012:cluster/prod"},
		},
		{
			action: "ListClusters",
			want:   []string{"*"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveARNs(t, tc.action, tc.params, tc.body))
		})
	}
}

// The access-entry discriminator must be the one the handler persists, or a
// Deny naming the entry fences an ARN no object ever carries.
func TestResourceARNsAccessEntryMatchesHandler(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/app/admin"
	want := handlers_eks.PrincipalARNHash(principal)

	resources := resolveARNs(t, "DeleteAccessEntry", []string{"prod", principal}, "")
	assert.Equal(t,
		[]string{"arn:aws:eks:ap-southeast-2:123456789012:access-entry/prod/" + want},
		resources)

	// CreateAccessEntry names the entry from the body and the cluster it lands in.
	resources = resolveARNs(t, "CreateAccessEntry", []string{"prod"},
		`{"principalArn":"`+principal+`"}`)
	assert.Equal(t, []string{
		"arn:aws:eks:ap-southeast-2:123456789012:cluster/prod",
		"arn:aws:eks:ap-southeast-2:123456789012:access-entry/prod/" + want,
	}, resources)
}

// Route captures arrive percent-decoded, so the ARN carries the decoded name
// the handler receives rather than the wire spelling.
func TestResourceARNsUsesDecodedPathParams(t *testing.T) {
	assert.Equal(t,
		[]string{"arn:aws:eks:ap-southeast-2:123456789012:cluster/my cluster"},
		resolveARNs(t, "DescribeCluster", []string{"my cluster"}, ""))
}

// The tag handler ignores the ARN's own region and account and works in the
// caller's bucket, so the gate must authorize the caller's account too.
func TestResourceARNsTagARNIsReanchored(t *testing.T) {
	tests := []struct {
		name        string
		resourceARN string
		want        []string
	}{
		{
			name:        "cluster",
			resourceARN: "arn:aws:eks:ap-southeast-2:123456789012:cluster/prod",
			want:        []string{"arn:aws:eks:ap-southeast-2:123456789012:cluster/prod"},
		},
		{
			name:        "foreign account is evaluated under the caller",
			resourceARN: "arn:aws:eks:us-east-1:999999999999:cluster/prod",
			want:        []string{"arn:aws:eks:ap-southeast-2:123456789012:cluster/prod"},
		},
		{
			name:        "nodegroup discriminator is re-derived, as the handler ignores it",
			resourceARN: "arn:aws:eks:ap-southeast-2:123456789012:nodegroup/prod/workers/8ac0f0b1",
			want:        []string{nodegroupARN("prod", "workers")},
		},
		{
			name:        "addon",
			resourceARN: "arn:aws:eks:ap-southeast-2:123456789012:addon/prod/coredns",
			want:        []string{"arn:aws:eks:ap-southeast-2:123456789012:addon/prod/coredns"},
		},
		{
			name:        "ARN the handler does not serve stays its own fault",
			resourceARN: "arn:aws:ec2:ap-southeast-2:123456789012:instance/i-abc",
			want:        []string{"*"},
		},
		{
			name:        "malformed ARN",
			resourceARN: "not-an-arn",
			want:        []string{"*"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveARNs(t, "TagResource", []string{tc.resourceARN}, ""))
		})
	}
}

// The internal control-plane routes carry the cluster's owning account in the
// path, and that is the account whose records the handler reads.
func TestResourceARNsInternalRoutesUsePathAccount(t *testing.T) {
	assert.Equal(t,
		[]string{"arn:aws:eks:ap-southeast-2:999999999999:cluster/prod"},
		resolveARNs(t, "ListInternalAddons", []string{"prod", "999999999999"}, ""))

	assert.Equal(t,
		[]string{"arn:aws:eks:ap-southeast-2:999999999999:cluster/prod"},
		resolveARNs(t, "GetRecoveryDirective", []string{"prod", "999999999999", "i-abc"}, ""))
}

// An identifier the gate cannot read authorizes account-wide, leaving the
// rejection to the handler that validates the request.
func TestResourceARNsUnresolvedIdentifierIsAccountWide(t *testing.T) {
	assert.Equal(t, []string{"*"}, resolveARNs(t, "DescribeCluster", nil, ""))
	assert.Equal(t, []string{"*"}, resolveARNs(t, "CreateCluster", nil, ""))
	assert.Equal(t, []string{"*"}, resolveARNs(t, "CreateCluster", nil, "{not json"))
	assert.Equal(t, []string{"*"}, resolveARNs(t, "CreateCluster", nil, `{"version":"1.31"}`))
	assert.Equal(t, []string{"*"}, resolveARNs(t, "DescribeNodegroup", []string{"prod"}, ""))

	// A create whose child is unnamed still evaluates the cluster: the absent
	// member drops out instead of widening the whole check to "*".
	assert.Equal(t,
		[]string{"arn:aws:eks:ap-southeast-2:123456789012:cluster/prod"},
		resolveARNs(t, "CreateNodegroup", []string{"prod"}, `{}`))
}

// An id whose value is literally "*" builds an ARN ending in "*", which is a
// value and not a pattern, so it cannot widen a grant.
func TestResourceARNsIdentifierIsAValue(t *testing.T) {
	assert.Equal(t,
		[]string{"arn:aws:eks:ap-southeast-2:123456789012:cluster/*"},
		resolveARNs(t, "DescribeCluster", []string{"*"}, ""))
}

func TestResourceARNsUnknownActionFailsClosed(t *testing.T) {
	_, err := gateway_eks.ResourceARNs("NotAnAction", authzRegion, authzAccountID, nil, nil)
	require.Error(t, err)
}
