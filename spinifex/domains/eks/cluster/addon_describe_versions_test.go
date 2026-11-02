package cluster

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/addon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func describeVersions(t *testing.T, svc *EKSServiceImpl, in *eks.DescribeAddonVersionsInput) *eks.DescribeAddonVersionsOutput {
	t.Helper()
	out, err := svc.DescribeAddonVersions(context.Background(), in, testAccountID)
	require.NoError(t, err)
	return out
}

// The terraform aws_eks_addon_version data source asks for one add-on at the
// cluster's Kubernetes version, then takes the version whose compatibility for
// that cluster version is the default (or, with most_recent, the first one).
func TestDescribeAddonVersions_TerraformProviderCall(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	out := describeVersions(t, svc, &eks.DescribeAddonVersionsInput{
		AddonName:         aws.String(csiAddon),
		KubernetesVersion: aws.String(defaultK8sVersion),
	})
	require.Len(t, out.Addons, 1)
	info := out.Addons[0]
	assert.Equal(t, csiAddon, aws.StringValue(info.AddonName))
	assert.Nil(t, out.NextToken)

	spec, ok := addon.Lookup(csiAddon)
	require.True(t, ok)
	require.Equal(t, spec.AWSVersions(), versionStrings(info), "newest first, AWS form")

	defaults := 0
	for _, v := range info.AddonVersions {
		require.Len(t, v.Compatibilities, 1, "one entry, for the requested cluster version")
		c := v.Compatibilities[0]
		assert.Equal(t, defaultK8sVersion, aws.StringValue(c.ClusterVersion))
		assert.Equal(t, []string{"*"}, aws.StringValueSlice(c.PlatformVersions))
		if aws.BoolValue(c.DefaultVersion) {
			defaults++
			assert.Equal(t, spec.AWSVersion(spec.DefaultVersion), aws.StringValue(v.AddonVersion))
		}
	}
	assert.Equal(t, 1, defaults, "exactly one defaultVersion true")
}

// AWS observed (EKS-01): compatibilities holds only the requested version. The
// response for a version no add-on supports is not captured; this pins the
// shape #61 chose, an empty list with no error.
func TestDescribeAddonVersions_UnsupportedKubernetesVersionIsEmpty(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	out := describeVersions(t, svc, &eks.DescribeAddonVersionsInput{
		AddonName:         aws.String(csiAddon),
		KubernetesVersion: aws.String("1.29"),
	})
	assert.Empty(t, out.Addons)
	assert.Nil(t, out.NextToken)
}

func TestDescribeAddonVersions_FiltersByNameAndHidesFixturesUnlessNamed(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	for _, name := range []string{csiAddon, "argocd", albController} {
		out := describeVersions(t, svc, &eks.DescribeAddonVersionsInput{AddonName: aws.String(name)})
		require.Len(t, out.Addons, 1, name)
		assert.Equal(t, name, aws.StringValue(out.Addons[0].AddonName))
	}
	assert.Empty(t, describeVersions(t, svc, &eks.DescribeAddonVersionsInput{AddonName: aws.String("vpc-cni")}).Addons)
}

// maxResults counts add-ons, not versions; a page boundary never splits an
// add-on's version list, and the last page carries no token.
func TestDescribeAddonVersions_PagesByAddon(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	all := describeVersions(t, svc, &eks.DescribeAddonVersionsInput{})
	require.Greater(t, len(all.Addons), 1)
	assert.Nil(t, all.NextToken)

	var names []string
	in := &eks.DescribeAddonVersionsInput{MaxResults: aws.Int64(1)}
	for range len(all.Addons) {
		out := describeVersions(t, svc, in)
		require.Len(t, out.Addons, 1)
		names = append(names, aws.StringValue(out.Addons[0].AddonName))
		assert.Equal(t, versionStrings(all.Addons[len(names)-1]), versionStrings(out.Addons[0]))
		in.NextToken = out.NextToken
	}
	assert.Nil(t, in.NextToken, "last page carries no token")
	var want []string
	for _, a := range all.Addons {
		want = append(want, aws.StringValue(a.AddonName))
	}
	assert.Equal(t, want, names)
}

// Every add-on carries the descriptive fields the data source and console read.
func TestDescribeAddonVersions_CarriesAddonAndVersionMetadata(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	for _, a := range describeVersions(t, svc, &eks.DescribeAddonVersionsInput{}).Addons {
		name := aws.StringValue(a.AddonName)
		assert.NotEmpty(t, aws.StringValue(a.Type), name)
		assert.NotEmpty(t, aws.StringValue(a.Owner), name)
		assert.NotEmpty(t, aws.StringValue(a.Publisher), name)
		assert.Nil(t, a.MarketplaceInformation, name)
		for _, v := range a.AddonVersions {
			assert.NotEmpty(t, v.Architecture, name)
			assert.NotNil(t, v.RequiresConfiguration, name)
			assert.NotNil(t, v.RequiresIamPermissions, name)
		}
	}
}

// types, publishers and owners are model filters; each keeps add-ons whose
// field matches one of the given values.
func TestDescribeAddonVersions_FiltersByTypePublisherOwner(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	first := describeVersions(t, svc, &eks.DescribeAddonVersionsInput{}).Addons[0]
	cases := []struct {
		name  string
		match func(*eks.DescribeAddonVersionsInput, []*string)
		value *string
	}{
		{"types", func(in *eks.DescribeAddonVersionsInput, v []*string) { in.Types = v }, first.Type},
		{"publishers", func(in *eks.DescribeAddonVersionsInput, v []*string) { in.Publishers = v }, first.Publisher},
		{"owners", func(in *eks.DescribeAddonVersionsInput, v []*string) { in.Owners = v }, first.Owner},
	}
	for _, c := range cases {
		hit := &eks.DescribeAddonVersionsInput{}
		c.match(hit, aws.StringSlice([]string{"nobody", aws.StringValue(c.value)}))
		out := describeVersions(t, svc, hit)
		require.NotEmpty(t, out.Addons, c.name)
		assert.Contains(t, addonNames(out), aws.StringValue(first.AddonName), c.name)

		miss := &eks.DescribeAddonVersionsInput{}
		c.match(miss, aws.StringSlice([]string{"nobody"}))
		assert.Empty(t, describeVersions(t, svc, miss).Addons, c.name)
	}
}

func versionStrings(a *eks.AddonInfo) []string {
	out := make([]string, 0, len(a.AddonVersions))
	for _, v := range a.AddonVersions {
		out = append(out, aws.StringValue(v.AddonVersion))
	}
	return out
}

func addonNames(o *eks.DescribeAddonVersionsOutput) []string {
	out := make([]string, 0, len(o.Addons))
	for _, a := range o.Addons {
		out = append(out, aws.StringValue(a.AddonName))
	}
	return out
}
