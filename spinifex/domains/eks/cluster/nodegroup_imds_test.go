package cluster

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLaunchTemplates answers DescribeLaunchTemplateVersions with a canned
// output or error and records every request it receives.
type fakeLaunchTemplates struct {
	out   *ec2.DescribeLaunchTemplateVersionsOutput
	err   error
	calls []*ec2.DescribeLaunchTemplateVersionsInput
	accts []string
}

var _ launchTemplateVersionReader = (*fakeLaunchTemplates)(nil)

func (f *fakeLaunchTemplates) DescribeLaunchTemplateVersions(_ context.Context, in *ec2.DescribeLaunchTemplateVersionsInput, accountID string) (*ec2.DescribeLaunchTemplateVersionsOutput, error) {
	f.calls = append(f.calls, in)
	f.accts = append(f.accts, accountID)
	return f.out, f.err
}

func templateVersionWith(opts *ec2.LaunchTemplateInstanceMetadataOptions) *ec2.DescribeLaunchTemplateVersionsOutput {
	return &ec2.DescribeLaunchTemplateVersionsOutput{
		LaunchTemplateVersions: []*ec2.LaunchTemplateVersion{{
			LaunchTemplateData: &ec2.ResponseLaunchTemplateData{MetadataOptions: opts},
		}},
	}
}

// launchWorkerWith drives launchOneWorker for a nodegroup that names lt (nil for
// none) and returns the service fixture plus the launch error.
func launchWorkerWith(t *testing.T, lt *NodegroupLaunchTemplate, reader launchTemplateVersionReader) (*eksServiceFixture, error) {
	t.Helper()
	f := newEKSServiceFixture(t)
	f.svc.deps.LaunchTemplates = reader
	rec := &NodegroupRecord{
		ClusterName:    "c1",
		Name:           "ng1",
		Subnets:        []string{"subnet-aaa"},
		LaunchTemplate: lt,
	}
	meta := &ClusterMeta{Name: "c1", Endpoint: "https://10.254.0.9:443"}
	_, err := f.svc.launchOneWorker(t.Context(), rec, meta, "sg-ng", "ami-eks", "tok", testAccountID)
	return f, err
}

func launchedMetadata(t *testing.T, f *eksServiceFixture) *ec2.InstanceMetadataOptionsRequest {
	t.Helper()
	require.Len(t, f.worker.runCalls, 1)
	return f.worker.runCalls[0].MetadataOptions
}

// Managed node groups launch workers with IMDSv2 required and a hop limit of 2,
// so a pod one routed hop behind the node can still reach the token endpoint.
func TestLaunchOneWorker_NoTemplateUsesManagedNodegroupMetadataDefault(t *testing.T) {
	t.Parallel()
	f, err := launchWorkerWith(t, nil, nil)
	require.NoError(t, err)

	opts := launchedMetadata(t, f)
	require.NotNil(t, opts, "worker RunInstancesInput carries no MetadataOptions")
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(opts.HttpTokens))
	assert.Equal(t, int64(2), aws.Int64Value(opts.HttpPutResponseHopLimit))
}

func TestLaunchOneWorker_TemplateMetadataOptionsUsedAsGiven(t *testing.T) {
	t.Parallel()
	reader := &fakeLaunchTemplates{out: templateVersionWith(&ec2.LaunchTemplateInstanceMetadataOptions{
		HttpTokens:              aws.String(ec2.LaunchTemplateHttpTokensStateOptional),
		HttpPutResponseHopLimit: aws.Int64(3),
	})}
	f, err := launchWorkerWith(t, &NodegroupLaunchTemplate{ID: "lt-0123", Version: "2"}, reader)
	require.NoError(t, err)

	opts := launchedMetadata(t, f)
	require.NotNil(t, opts)
	assert.Equal(t, ec2.HttpTokensStateOptional, aws.StringValue(opts.HttpTokens))
	assert.Equal(t, int64(3), aws.Int64Value(opts.HttpPutResponseHopLimit))
}

func TestLaunchOneWorker_TemplateWithoutMetadataOptionsUsesDefault(t *testing.T) {
	t.Parallel()
	reader := &fakeLaunchTemplates{out: templateVersionWith(nil)}
	f, err := launchWorkerWith(t, &NodegroupLaunchTemplate{ID: "lt-0123", Version: "1"}, reader)
	require.NoError(t, err)

	opts := launchedMetadata(t, f)
	require.NotNil(t, opts)
	assert.Equal(t, ec2.HttpTokensStateRequired, aws.StringValue(opts.HttpTokens))
	assert.Equal(t, int64(2), aws.Int64Value(opts.HttpPutResponseHopLimit))
}

// A template that cannot be resolved fails the launch with the error EC2
// RunInstances returns, and no worker is started with a silent default.
func TestLaunchOneWorker_MissingTemplateVersionFailsLikeRunInstances(t *testing.T) {
	t.Parallel()
	cases := map[string]*ec2.DescribeLaunchTemplateVersionsOutput{
		"no versions":         {},
		"version has no data": {LaunchTemplateVersions: []*ec2.LaunchTemplateVersion{{}}},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, err := launchWorkerWith(t, &NodegroupLaunchTemplate{ID: "lt-0123", Version: "9"}, &fakeLaunchTemplates{out: out})
			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound), "got %v", err)
			assert.Empty(t, f.worker.runCalls)
		})
	}
}

func TestLaunchOneWorker_TemplateLookupErrorFailsLaunch(t *testing.T) {
	t.Parallel()
	lookupErr := errors.New(awserrors.ErrorInvalidLaunchTemplateIdNotFound)
	f, err := launchWorkerWith(t, &NodegroupLaunchTemplate{ID: "lt-gone"}, &fakeLaunchTemplates{err: lookupErr})
	require.ErrorIs(t, err, lookupErr)
	assert.Empty(t, f.worker.runCalls)
}

// Naming a template with no reader wired must not quietly launch with the
// default and ignore the template.
func TestLaunchOneWorker_TemplateNamedButNoReaderFails(t *testing.T) {
	t.Parallel()
	f, err := launchWorkerWith(t, &NodegroupLaunchTemplate{ID: "lt-0123", Version: "1"}, nil)
	require.Error(t, err)
	assert.Empty(t, f.worker.runCalls)
}

func TestLaunchOneWorker_TemplateRequestShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		lt          *NodegroupLaunchTemplate
		wantID      string
		wantName    string
		wantVersion string
	}{
		{"id with explicit version", &NodegroupLaunchTemplate{ID: "lt-0123", Version: "3"}, "lt-0123", "", "3"},
		{"id without version uses $Default", &NodegroupLaunchTemplate{ID: "lt-0123"}, "lt-0123", "", "$Default"},
		{"name with $Latest", &NodegroupLaunchTemplate{Name: "ng-template", Version: "$Latest"}, "", "ng-template", "$Latest"},
		{"name without version uses $Default", &NodegroupLaunchTemplate{Name: "ng-template"}, "", "ng-template", "$Default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeLaunchTemplates{out: templateVersionWith(nil)}
			_, err := launchWorkerWith(t, tc.lt, reader)
			require.NoError(t, err)

			require.Len(t, reader.calls, 1)
			in := reader.calls[0]
			assert.Equal(t, tc.wantID, aws.StringValue(in.LaunchTemplateId))
			assert.Equal(t, tc.wantName, aws.StringValue(in.LaunchTemplateName))
			assert.Equal(t, []string{tc.wantVersion}, aws.StringValueSlice(in.Versions))
			assert.Equal(t, []string{testAccountID}, reader.accts, "template is read in the caller's account")
		})
	}
}

// A template that sets only some metadata fields keeps those as given and takes
// the managed default (tokens required, hop limit 2) for the rest.
func TestLaunchOneWorker_TemplateMetadataGapsTakeTheManagedDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		opts       *ec2.LaunchTemplateInstanceMetadataOptions
		wantTokens string
		wantHops   int64
	}{
		{"only tokens", &ec2.LaunchTemplateInstanceMetadataOptions{HttpTokens: aws.String(ec2.LaunchTemplateHttpTokensStateOptional)}, ec2.HttpTokensStateOptional, 2},
		{"only hop limit", &ec2.LaunchTemplateInstanceMetadataOptions{HttpPutResponseHopLimit: aws.Int64(4)}, ec2.HttpTokensStateRequired, 4},
		{"only endpoint", &ec2.LaunchTemplateInstanceMetadataOptions{HttpEndpoint: aws.String(ec2.LaunchTemplateInstanceMetadataEndpointStateEnabled)}, ec2.HttpTokensStateRequired, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeLaunchTemplates{out: templateVersionWith(tc.opts)}
			f, err := launchWorkerWith(t, &NodegroupLaunchTemplate{ID: "lt-0123", Version: "1"}, reader)
			require.NoError(t, err)

			opts := launchedMetadata(t, f)
			require.NotNil(t, opts)
			assert.Equal(t, tc.wantTokens, aws.StringValue(opts.HttpTokens))
			assert.Equal(t, tc.wantHops, aws.Int64Value(opts.HttpPutResponseHopLimit))
		})
	}
}

func TestLaunchOneWorker_EveryTemplateMetadataFieldIsCarriedThrough(t *testing.T) {
	t.Parallel()
	reader := &fakeLaunchTemplates{out: templateVersionWith(&ec2.LaunchTemplateInstanceMetadataOptions{
		HttpTokens:              aws.String(ec2.LaunchTemplateHttpTokensStateOptional),
		HttpPutResponseHopLimit: aws.Int64(3),
		HttpEndpoint:            aws.String(ec2.LaunchTemplateInstanceMetadataEndpointStateEnabled),
		HttpProtocolIpv6:        aws.String(ec2.LaunchTemplateInstanceMetadataProtocolIpv6Disabled),
		InstanceMetadataTags:    aws.String(ec2.LaunchTemplateInstanceMetadataTagsStateDisabled),
	})}
	f, err := launchWorkerWith(t, &NodegroupLaunchTemplate{Name: "ng-template", Version: "1"}, reader)
	require.NoError(t, err)

	assert.Equal(t, &ec2.InstanceMetadataOptionsRequest{
		HttpTokens:              aws.String(ec2.HttpTokensStateOptional),
		HttpPutResponseHopLimit: aws.Int64(3),
		HttpEndpoint:            aws.String(ec2.InstanceMetadataEndpointStateEnabled),
		HttpProtocolIpv6:        aws.String(ec2.InstanceMetadataProtocolStateDisabled),
		InstanceMetadataTags:    aws.String(ec2.InstanceMetadataTagsStateDisabled),
	}, launchedMetadata(t, f))
}

// RunInstances rejects InstanceMetadataTags=enabled, and workers launch through
// it. The template's value must reach the launcher unaltered and the launcher's
// UnsupportedOperation must fail the launch, never be dropped silently.
func TestLaunchOneWorker_TemplateTagsEnabledSurfacesUnsupportedOperation(t *testing.T) {
	t.Parallel()
	reader := &fakeLaunchTemplates{out: templateVersionWith(&ec2.LaunchTemplateInstanceMetadataOptions{
		InstanceMetadataTags: aws.String(ec2.LaunchTemplateInstanceMetadataTagsStateEnabled),
	})}
	f := newEKSServiceFixture(t)
	f.svc.deps.LaunchTemplates = reader
	f.worker.runErr = errors.New(awserrors.ErrorUnsupportedOperation)
	f.worker.failNextN = 1
	rec := &NodegroupRecord{ClusterName: "c1", Name: "ng1", Subnets: []string{"subnet-aaa"}, LaunchTemplate: &NodegroupLaunchTemplate{ID: "lt-0123", Version: "1"}}

	_, err := f.svc.launchOneWorker(t.Context(), rec, &ClusterMeta{Name: "c1"}, "sg-ng", "ami-eks", "tok", testAccountID)

	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorUnsupportedOperation), "got %v", err)
	require.Len(t, f.worker.runCalls, 1)
	assert.Equal(t, ec2.InstanceMetadataTagsStateEnabled, aws.StringValue(f.worker.runCalls[0].MetadataOptions.InstanceMetadataTags))
}
