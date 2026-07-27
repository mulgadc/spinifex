package handlers_eks

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAddonInstaller records calls so tests can assert the state machine drives
// the installer with the right record.
type fakeAddonInstaller struct {
	installs   []*AddonRecord
	uninstalls []string
	installErr error
}

var _ AddonInstaller = (*fakeAddonInstaller)(nil)

func (f *fakeAddonInstaller) Install(_ context.Context, _, _ string, rec *AddonRecord) error {
	f.installs = append(f.installs, rec)
	return f.installErr
}

func (f *fakeAddonInstaller) Uninstall(_ context.Context, _, _, addon string) error {
	f.uninstalls = append(f.uninstalls, addon)
	return nil
}

func setupAddonService(t *testing.T) (*EKSServiceImpl, *fakeAddonInstaller) {
	t.Helper()
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	fake := &fakeAddonInstaller{}
	svc.deps.AddonInstaller = fake
	return svc, fake
}

const albController = "aws-load-balancer-controller"

func TestDescribeAddonVersions_ReturnsCatalog(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.DescribeAddonVersions(context.Background(), &eks.DescribeAddonVersionsInput{}, testAccountID)
	require.NoError(t, err)
	visible := 0
	for _, spec := range addonCatalog {
		if !spec.Hidden {
			visible++
		}
	}
	assert.Len(t, out.Addons, visible)
	for _, a := range out.Addons {
		assert.NotEqual(t, "spinifex-noop", aws.StringValue(a.AddonName), "hidden fixture must not surface")
	}

	// Filtered to one addon.
	out, err = svc.DescribeAddonVersions(context.Background(), &eks.DescribeAddonVersionsInput{
		AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Addons, 1)
	assert.Equal(t, albController, aws.StringValue(out.Addons[0].AddonName))
	require.NotEmpty(t, out.Addons[0].AddonVersions)
	assert.True(t, aws.BoolValue(out.Addons[0].AddonVersions[0].RequiresIamPermissions))
}

func TestCreateAddon_UnknownClusterIsNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("missing"), AddonName: aws.String(albController),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestCreateAddon_UnknownAddonRejected(t *testing.T) {
	svc, _ := setupAddonService(t)
	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("not-a-real-addon"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateAddon_UnknownVersionRejected(t *testing.T) {
	svc, _ := setupAddonService(t)
	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
		AddonVersion: aws.String("9.9.9-nope"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateAddon_CreatesStagingAndDefaultsVersion(t *testing.T) {
	svc, fake := setupAddonService(t)
	spec, _ := lookupAddon(albController)

	out, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Addon)
	// Version defaults to catalog default; status starts CREATING (honest until
	// VM-side delivery confirms ACTIVE).
	assert.Equal(t, spec.DefaultVersion, aws.StringValue(out.Addon.AddonVersion))
	assert.Equal(t, string(AddonStatusCreating), aws.StringValue(out.Addon.Status))
	assert.Contains(t, aws.StringValue(out.Addon.AddonArn), ":addon/c1/"+albController)

	// Installer was driven with the persisted record.
	require.Len(t, fake.installs, 1)
	assert.Equal(t, albController, fake.installs[0].AddonName)

	// Duplicate create → ResourceInUseException.
	_, err = svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceInUse)
}

func TestCreateAddon_InstallerFailureMarksCreateFailed(t *testing.T) {
	svc, fake := setupAddonService(t)
	fake.installErr = errors.New("delivery bus unreachable")

	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.Error(t, err)

	desc, err := svc.DescribeAddon(context.Background(), &eks.DescribeAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, string(AddonStatusCreateFailed), aws.StringValue(desc.Addon.Status))
}

func TestAddon_DescribeListDelete(t *testing.T) {
	svc, fake := setupAddonService(t)

	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeAddon(context.Background(), &eks.DescribeAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, albController, aws.StringValue(desc.Addon.AddonName))

	list, err := svc.ListAddons(context.Background(), &eks.ListAddonsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{albController}, aws.StringValueSlice(list.Addons))

	_, err = svc.DeleteAddon(context.Background(), &eks.DeleteAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{albController}, fake.uninstalls)

	_, err = svc.DescribeAddon(context.Background(), &eks.DescribeAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestDescribeDeleteAddon_MissingIsNotFound(t *testing.T) {
	svc, _ := setupAddonService(t)
	_, err := svc.DescribeAddon(context.Background(), &eks.DescribeAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
	_, err = svc.DeleteAddon(context.Background(), &eks.DeleteAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestUpdateAddon_ChangesVersionAndReinstalls(t *testing.T) {
	svc, fake := setupAddonService(t)
	spec, _ := lookupAddon("argocd")

	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, fake.installs, 1)

	out, err := svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
		ConfigurationValues: aws.String(`{"replicaCount":2}`),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Update)
	assert.Equal(t, eks.UpdateTypeAddonUpdate, aws.StringValue(out.Update.Type))
	require.Len(t, fake.installs, 2, "update must re-drive the installer")

	desc, err := svc.DescribeAddon(context.Background(), &eks.DescribeAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, `{"replicaCount":2}`, aws.StringValue(desc.Addon.ConfigurationValues))
	assert.Equal(t, spec.DefaultVersion, aws.StringValue(desc.Addon.AddonVersion))
	assert.Equal(t, string(AddonStatusUpdating), aws.StringValue(desc.Addon.Status))
}

func TestUpdateAddon_UnknownVersionRejected(t *testing.T) {
	svc, _ := setupAddonService(t)
	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
		AddonVersion: aws.String("9.9.9-nope"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestUpdateAddon_MissingIsNotFound(t *testing.T) {
	svc, _ := setupAddonService(t)
	_, err := svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

// The default (uninjected) installer stages a manifest in KV that the VM-side
// delivery slice consumes.
func TestStagingInstaller_StagesManifest(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")

	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
		ServiceAccountRoleArn: aws.String("arn:aws:iam::111122223333:role/alb"),
	}, testAccountID)
	require.NoError(t, err)

	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID, 1)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), AddonManifestKey("c1", albController))
	require.NoError(t, err, "installer must stage a manifest for VM-side delivery")
	assert.Contains(t, string(entry.Value()), albController)
}

func TestListStagedAddonManifests(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")

	// Empty cluster: no staged manifests, no error.
	out, err := svc.ListStagedAddonManifests(context.Background(), &ListStagedAddonManifestsInput{ClusterName: "c1"}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Manifests)

	// Stage two addons; reader returns both, sorted by name, with config carried.
	_, err = svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
		ServiceAccountRoleArn: aws.String("arn:aws:iam::111122223333:role/alb"),
	}, testAccountID)
	require.NoError(t, err)

	out, err = svc.ListStagedAddonManifests(context.Background(), &ListStagedAddonManifestsInput{ClusterName: "c1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Manifests, 2)
	assert.Equal(t, "argocd", out.Manifests[0].AddonName, "sorted by addon name")
	assert.Equal(t, albController, out.Manifests[1].AddonName)
	assert.Equal(t, "arn:aws:iam::111122223333:role/alb", out.Manifests[1].ServiceAccountRoleArn)

	// Deleting an addon unstages its manifest; reader drops it.
	_, err = svc.DeleteAddon(context.Background(), &eks.DeleteAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(albController),
	}, testAccountID)
	require.NoError(t, err)

	out, err = svc.ListStagedAddonManifests(context.Background(), &ListStagedAddonManifestsInput{ClusterName: "c1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Manifests, 1)
	assert.Equal(t, "argocd", out.Manifests[0].AddonName)

	// Unknown cluster surfaces ResourceNotFound; empty cluster name is invalid.
	_, err = svc.ListStagedAddonManifests(context.Background(), &ListStagedAddonManifestsInput{ClusterName: "missing"}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
	_, err = svc.ListStagedAddonManifests(context.Background(), &ListStagedAddonManifestsInput{ClusterName: ""}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}
