package cluster

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/domains/eks/addon"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	csiAddon       = "aws-ebs-csi-driver"
	csiBare        = "1.40.1"
	csiAWSVersion  = "v1.40.1-eksbuild.1"
	csiWrongBuild  = "v1.40.1-eksbuild.2"
	versionParam   = "addonVersion"
	notBeforeAWSOK = "an AWS-format version must be accepted"
)

func TestDescribeAddonVersions_ReturnsAWSFormNewestFirst(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	out, err := svc.DescribeAddonVersions(context.Background(), &eks.DescribeAddonVersionsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotEmpty(t, out.Addons)
	for _, a := range out.Addons {
		spec, ok := addon.Lookup(aws.StringValue(a.AddonName))
		require.True(t, ok)
		require.Len(t, a.AddonVersions, len(spec.Versions))
		for i, v := range a.AddonVersions {
			assert.Equal(t, "v"+spec.Versions[i]+"-eksbuild.1", aws.StringValue(v.AddonVersion),
				"%s version %d", spec.Name, i)
		}
	}
}

func TestCreateAddon_AcceptsBothFormsAndStoresTheBareForm(t *testing.T) {
	t.Parallel()
	for name, input := range map[string]string{"aws form": csiAWSVersion, "bare form": csiBare} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, fake := setupAddonService(t)

			out, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
				ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
				AddonVersion: aws.String(input),
			}, testAccountID)
			require.NoError(t, err, notBeforeAWSOK)
			assert.Equal(t, csiAWSVersion, aws.StringValue(out.Addon.AddonVersion), "response is in AWS form")

			rec, err := addon.Get(t.Context(), acctKVForTest(t, svc), "c1", csiAddon)
			require.NoError(t, err)
			assert.Equal(t, csiBare, rec.AddonVersion, "the record keeps the upstream form")
			require.Len(t, fake.installs, 1)
			assert.Equal(t, csiBare, fake.installs[0].AddonVersion, "the bundle lookup sees the upstream form")
		})
	}
}

func TestCreateAddon_DefaultVersionResponseIsAWSForm(t *testing.T) {
	t.Parallel()
	svc, _ := setupAddonService(t)
	out, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, csiAWSVersion, aws.StringValue(out.Addon.AddonVersion))
}

func TestUpdateAddon_AcceptsBothFormsAndStoresTheBareForm(t *testing.T) {
	t.Parallel()
	for name, input := range map[string]string{"aws form": csiAWSVersion, "bare form": csiBare} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, fake := setupAddonService(t)
			_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
				ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
			}, testAccountID)
			require.NoError(t, err)

			_, err = svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
				ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
				AddonVersion: aws.String(input),
			}, testAccountID)
			require.NoError(t, err, notBeforeAWSOK)

			rec, err := addon.Get(t.Context(), acctKVForTest(t, svc), "c1", csiAddon)
			require.NoError(t, err)
			assert.Equal(t, csiBare, rec.AddonVersion)
			require.Len(t, fake.installs, 2)
			assert.Equal(t, csiBare, fake.installs[1].AddonVersion)
		})
	}
}

func TestDescribeAddon_ReturnsAWSFormForAnyStoredForm(t *testing.T) {
	t.Parallel()
	for name, stored := range map[string]string{
		"record stored bare before the change": csiBare,
		"record already in AWS form":           csiAWSVersion,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, _ := setupAddonService(t)
			_, err := svc.addons().Create(context.Background(), acctKVForTest(t, svc), testAccountID, addon.Desired{
				Cluster: "c1", Name: csiAddon, Version: stored,
			})
			require.NoError(t, err)

			desc, err := svc.DescribeAddon(context.Background(), &eks.DescribeAddonInput{
				ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
			}, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, csiAWSVersion, aws.StringValue(desc.Addon.AddonVersion), "no double wrap")

			list, err := svc.ListAddons(context.Background(), &eks.ListAddonsInput{ClusterName: aws.String("c1")}, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, []string{csiAddon}, aws.StringValueSlice(list.Addons))
		})
	}
}

func TestUpdateAddon_ResponseAndRecordAfterBareRecord(t *testing.T) {
	t.Parallel()
	svc, _ := setupAddonService(t)
	_, err := svc.addons().Create(context.Background(), acctKVForTest(t, svc), testAccountID, addon.Desired{
		Cluster: "c1", Name: csiAddon, Version: csiBare,
	})
	require.NoError(t, err)

	_, err = svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
		ConfigurationValues: aws.String(`{"a":1}`),
	}, testAccountID)
	require.NoError(t, err)
	desc, err := svc.DescribeAddon(context.Background(), &eks.DescribeAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, csiAWSVersion, aws.StringValue(desc.Addon.AddonVersion))
}

func TestAddonVersionRefusal_NamesTheParameterAndListsSupportedValues(t *testing.T) {
	t.Parallel()
	svc, _ := setupAddonService(t)
	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
	}, testAccountID)
	require.NoError(t, err)

	refusals := map[string]func() error{
		"create": func() error {
			_, e := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
				ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
				AddonVersion: aws.String(csiWrongBuild),
			}, testAccountID)
			return e
		},
		"update": func() error {
			_, e := svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
				ClusterName: aws.String("c1"), AddonName: aws.String("argocd"),
				AddonVersion: aws.String("v3.0.23-eksbuild.2"),
			}, testAccountID)
			return e
		},
	}
	want := map[string]string{"create": csiAWSVersion, "update": "v3.0.23-eksbuild.1"}
	for name, refuse := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := refuse()
			require.Error(t, err)
			code, message, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, code, "this change leaves the error code alone")
			assert.Contains(t, message, versionParam)
			assert.Contains(t, message, want[name], "the supported AWS-form value is listed")
		})
	}
}

func TestAddonBareVersionInput_LogsItsUseAtInfo(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	svc, _ := setupAddonService(t)
	_, err := svc.CreateAddon(context.Background(), &eks.CreateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
	}, testAccountID)
	require.NoError(t, err)

	buf.Reset()
	_, err = svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
		AddonVersion: aws.String(csiBare),
	}, testAccountID)
	require.NoError(t, err)
	logged := buf.String()
	assert.Contains(t, logged, "level=INFO")
	assert.Contains(t, logged, "bare")
	assert.Contains(t, logged, csiAddon)
	assert.Contains(t, logged, "c1")

	buf.Reset()
	_, err = svc.UpdateAddon(context.Background(), &eks.UpdateAddonInput{
		ClusterName: aws.String("c1"), AddonName: aws.String(csiAddon),
		AddonVersion: aws.String(csiAWSVersion),
	}, testAccountID)
	require.NoError(t, err, notBeforeAWSOK)
	assert.NotContains(t, buf.String(), "bare", "the AWS form is the intended form and logs nothing")
}
