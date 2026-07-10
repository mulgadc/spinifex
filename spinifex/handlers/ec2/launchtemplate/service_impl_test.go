package handlers_ec2_launchtemplate

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccountID  = "123456789012"
	otherAccountID = "210987654321"
)

func setupTestService(t *testing.T) *LaunchTemplateServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := NewLaunchTemplateServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	return svc
}

func createTemplate(t *testing.T, svc *LaunchTemplateServiceImpl, name, instanceType string) *ec2.LaunchTemplate {
	t.Helper()
	out, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(name),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:      aws.String("ami-123"),
			InstanceType: aws.String(instanceType),
		},
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String(launchTemplateTagResourceType),
			Tags:         []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("test")}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	return out.LaunchTemplate
}

// --- CreateLaunchTemplate ---

func TestCreateLaunchTemplate_Basic(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")

	assert.Equal(t, "lt-", (*lt.LaunchTemplateId)[:3])
	assert.Equal(t, "web", aws.StringValue(lt.LaunchTemplateName))
	assert.Equal(t, int64(1), aws.Int64Value(lt.DefaultVersionNumber))
	assert.Equal(t, int64(1), aws.Int64Value(lt.LatestVersionNumber))
	require.Len(t, lt.Tags, 1)
	assert.Equal(t, "env", aws.StringValue(lt.Tags[0].Key))
}

func TestCreateLaunchTemplate_MissingData(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestCreateLaunchTemplate_NameMalformed(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("ab"), // < 3 chars
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{ImageId: aws.String("ami-1")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateNameMalformedException, err.Error())
}

func TestCreateLaunchTemplate_DuplicateName(t *testing.T) {
	svc := setupTestService(t)
	createTemplate(t, svc, "dup", "t3.micro")
	_, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("dup"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{ImageId: aws.String("ami-1")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateNameAlreadyExistsException, err.Error())
}

func TestCreateLaunchTemplate_DryRunNoPersist(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		DryRun:             aws.Bool(true),
		LaunchTemplateName: aws.String("dry"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{ImageId: aws.String("ami-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, out.LaunchTemplate)

	// Nothing persisted: the name is still claimable.
	createTemplate(t, svc, "dry", "t3.micro")
}

// TestCreateLaunchTemplate_OrphanNameReclaim verifies repair-on-write: a name
// whose header is gone (crash orphan) is reclaimed by the next create.
func TestCreateLaunchTemplate_OrphanNameReclaim(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "orphan", "t3.micro")

	// Simulate a crash after the name claim but with the header lost.
	require.NoError(t, svc.kv.Delete(headerKey(testAccountID, aws.StringValue(lt.LaunchTemplateId))))

	// The name index still points at the now-orphaned id; create must reclaim it.
	out, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("orphan"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{ImageId: aws.String("ami-2")},
	}, testAccountID)
	require.NoError(t, err)
	assert.NotEqual(t, aws.StringValue(lt.LaunchTemplateId), aws.StringValue(out.LaunchTemplate.LaunchTemplateId))
}

// --- DescribeLaunchTemplates ---

func TestDescribeLaunchTemplates_ByName(t *testing.T) {
	svc := setupTestService(t)
	createTemplate(t, svc, "web", "t3.micro")
	createTemplate(t, svc, "dbx", "t3.large")

	out, err := svc.DescribeLaunchTemplates(context.Background(), &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateNames: []*string{aws.String("web")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LaunchTemplates, 1)
	assert.Equal(t, "web", aws.StringValue(out.LaunchTemplates[0].LaunchTemplateName))
}

func TestDescribeLaunchTemplates_All(t *testing.T) {
	svc := setupTestService(t)
	createTemplate(t, svc, "web", "t3.micro")
	createTemplate(t, svc, "dbx", "t3.large")

	out, err := svc.DescribeLaunchTemplates(context.Background(), &ec2.DescribeLaunchTemplatesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.LaunchTemplates, 2)
}

func TestDescribeLaunchTemplates_UnknownName(t *testing.T) {
	svc := setupTestService(t)
	createTemplate(t, svc, "web", "t3.micro")
	_, err := svc.DescribeLaunchTemplates(context.Background(), &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateNames: []*string{aws.String("missing")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateNameNotFoundException, err.Error())
}

func TestDescribeLaunchTemplates_TagFilter(t *testing.T) {
	svc := setupTestService(t)
	createTemplate(t, svc, "web", "t3.micro")

	out, err := svc.DescribeLaunchTemplates(context.Background(), &ec2.DescribeLaunchTemplatesInput{
		Filters: []*ec2.Filter{{Name: aws.String("tag:env"), Values: []*string{aws.String("test")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.LaunchTemplates, 1)

	out, err = svc.DescribeLaunchTemplates(context.Background(), &ec2.DescribeLaunchTemplatesInput{
		Filters: []*ec2.Filter{{Name: aws.String("tag:env"), Values: []*string{aws.String("prod")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.LaunchTemplates)
}

func TestDescribeLaunchTemplates_AccountIsolation(t *testing.T) {
	svc := setupTestService(t)
	createTemplate(t, svc, "web", "t3.micro")

	out, err := svc.DescribeLaunchTemplates(context.Background(), &ec2.DescribeLaunchTemplatesInput{}, otherAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.LaunchTemplates)
}

// --- CreateLaunchTemplateVersion ---

func addVersion(t *testing.T, svc *LaunchTemplateServiceImpl, ltID, instanceType, sourceVersion string) *ec2.LaunchTemplateVersion {
	t.Helper()
	in := &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: aws.String(ltID),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			InstanceType: aws.String(instanceType),
		},
	}
	if sourceVersion != "" {
		in.SourceVersion = aws.String(sourceVersion)
	}
	out, err := svc.CreateLaunchTemplateVersion(context.Background(), in, testAccountID)
	require.NoError(t, err)
	return out.LaunchTemplateVersion
}

func TestCreateLaunchTemplateVersion_Increments(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)

	v2 := addVersion(t, svc, id, "t3.large", "")
	assert.Equal(t, int64(2), aws.Int64Value(v2.VersionNumber))
	assert.False(t, aws.BoolValue(v2.DefaultVersion), "new versions do not auto-become default")

	v3 := addVersion(t, svc, id, "t3.xlarge", "")
	assert.Equal(t, int64(3), aws.Int64Value(v3.VersionNumber))
}

func TestCreateLaunchTemplateVersion_SourceVersionMerge(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro") // v1: ImageId=ami-123, InstanceType=t3.micro
	id := aws.StringValue(lt.LaunchTemplateId)

	// v2 sources v1 and overrides only InstanceType; ImageId inherits.
	v2 := addVersion(t, svc, id, "t3.large", "1")
	assert.Equal(t, "t3.large", aws.StringValue(v2.LaunchTemplateData.InstanceType))
	assert.Equal(t, "ami-123", aws.StringValue(v2.LaunchTemplateData.ImageId), "unset field inherits source version")
}

func TestCreateLaunchTemplateVersion_NoSourceNoInherit(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)

	// Without SourceVersion the new version carries only the given data.
	v2 := addVersion(t, svc, id, "t3.large", "")
	assert.Equal(t, "t3.large", aws.StringValue(v2.LaunchTemplateData.InstanceType))
	assert.Nil(t, v2.LaunchTemplateData.ImageId, "no SourceVersion means no inheritance")
}

func TestCreateLaunchTemplateVersion_ConcurrentNumbering(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	nums := make(map[int64]bool)
	errs := make([]error, 0)
	for range n {
		wg.Go(func() {
			out, err := svc.CreateLaunchTemplateVersion(context.Background(), &ec2.CreateLaunchTemplateVersionInput{
				LaunchTemplateId:   aws.String(id),
				LaunchTemplateData: &ec2.RequestLaunchTemplateData{InstanceType: aws.String("t3.large")},
			}, testAccountID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			nums[aws.Int64Value(out.LaunchTemplateVersion.VersionNumber)] = true
		})
	}
	wg.Wait()

	require.Empty(t, errs, "no version create should fail under contention")
	assert.Len(t, nums, n, "every concurrent version got a unique number")
}

// --- DescribeLaunchTemplateVersions ---

func TestDescribeLaunchTemplateVersions_DefaultAndLatest(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)
	addVersion(t, svc, id, "t3.large", "")  // v2
	addVersion(t, svc, id, "t3.xlarge", "") // v3

	out, err := svc.DescribeLaunchTemplateVersions(context.Background(), &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id),
		Versions:         []*string{aws.String(versionDefault)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LaunchTemplateVersions, 1)
	assert.Equal(t, int64(1), aws.Int64Value(out.LaunchTemplateVersions[0].VersionNumber))
	assert.True(t, aws.BoolValue(out.LaunchTemplateVersions[0].DefaultVersion))

	out, err = svc.DescribeLaunchTemplateVersions(context.Background(), &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id),
		Versions:         []*string{aws.String(versionLatest)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LaunchTemplateVersions, 1)
	assert.Equal(t, int64(3), aws.Int64Value(out.LaunchTemplateVersions[0].VersionNumber))
}

func TestDescribeLaunchTemplateVersions_AllAndRange(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)
	addVersion(t, svc, id, "a", "")
	addVersion(t, svc, id, "b", "")
	addVersion(t, svc, id, "c", "") // versions 1..4

	out, err := svc.DescribeLaunchTemplateVersions(context.Background(), &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id),
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.LaunchTemplateVersions, 4)

	out, err = svc.DescribeLaunchTemplateVersions(context.Background(), &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id),
		MinVersion:       aws.String("2"),
		MaxVersion:       aws.String("3"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LaunchTemplateVersions, 2)
	assert.Equal(t, int64(2), aws.Int64Value(out.LaunchTemplateVersions[0].VersionNumber))
	assert.Equal(t, int64(3), aws.Int64Value(out.LaunchTemplateVersions[1].VersionNumber))
}

func TestDescribeLaunchTemplateVersions_MissingVersion(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	_, err := svc.DescribeLaunchTemplateVersions(context.Background(), &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: lt.LaunchTemplateId,
		Versions:         []*string{aws.String("99")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, err.Error())
}

func TestDescribeLaunchTemplateVersions_LatestAfterTailDelete(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)
	addVersion(t, svc, id, "a", "") // v2
	addVersion(t, svc, id, "b", "") // v3

	_, err := svc.DeleteLaunchTemplateVersions(context.Background(), &ec2.DeleteLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id),
		Versions:         []*string{aws.String("3")},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeLaunchTemplateVersions(context.Background(), &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id),
		Versions:         []*string{aws.String(versionLatest)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LaunchTemplateVersions, 1)
	assert.Equal(t, int64(2), aws.Int64Value(out.LaunchTemplateVersions[0].VersionNumber))
}

// --- ModifyLaunchTemplate ---

func TestModifyLaunchTemplate_SetDefault(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)
	addVersion(t, svc, id, "t3.large", "") // v2

	out, err := svc.ModifyLaunchTemplate(context.Background(), &ec2.ModifyLaunchTemplateInput{
		LaunchTemplateId: aws.String(id),
		DefaultVersion:   aws.String("2"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), aws.Int64Value(out.LaunchTemplate.DefaultVersionNumber))

	// $Default now resolves to v2.
	desc, err := svc.DescribeLaunchTemplateVersions(context.Background(), &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id),
		Versions:         []*string{aws.String(versionDefault)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), aws.Int64Value(desc.LaunchTemplateVersions[0].VersionNumber))
}

func TestModifyLaunchTemplate_DefaultToMissingVersion(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	_, err := svc.ModifyLaunchTemplate(context.Background(), &ec2.ModifyLaunchTemplateInput{
		LaunchTemplateId: lt.LaunchTemplateId,
		DefaultVersion:   aws.String("42"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, err.Error())
}

// --- DeleteLaunchTemplateVersions ---

func TestDeleteLaunchTemplateVersions_RejectDefault(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro") // default = v1
	out, err := svc.DeleteLaunchTemplateVersions(context.Background(), &ec2.DeleteLaunchTemplateVersionsInput{
		LaunchTemplateId: lt.LaunchTemplateId,
		Versions:         []*string{aws.String("1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.SuccessfullyDeletedLaunchTemplateVersions)
	require.Len(t, out.UnsuccessfullyDeletedLaunchTemplateVersions, 1)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue,
		aws.StringValue(out.UnsuccessfullyDeletedLaunchTemplateVersions[0].ResponseError.Code))
}

func TestDeleteLaunchTemplateVersions_SuccessAndMissing(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)
	addVersion(t, svc, id, "a", "") // v2

	out, err := svc.DeleteLaunchTemplateVersions(context.Background(), &ec2.DeleteLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id),
		Versions:         []*string{aws.String("2"), aws.String("9")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SuccessfullyDeletedLaunchTemplateVersions, 1)
	assert.Equal(t, int64(2), aws.Int64Value(out.SuccessfullyDeletedLaunchTemplateVersions[0].VersionNumber))
	require.Len(t, out.UnsuccessfullyDeletedLaunchTemplateVersions, 1)
	assert.Equal(t, int64(9), aws.Int64Value(out.UnsuccessfullyDeletedLaunchTemplateVersions[0].VersionNumber))
}

// --- DeleteLaunchTemplate ---

func TestDeleteLaunchTemplate_RemovesEverything(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	id := aws.StringValue(lt.LaunchTemplateId)
	addVersion(t, svc, id, "a", "")

	_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{
		LaunchTemplateId: aws.String(id),
	}, testAccountID)
	require.NoError(t, err)

	// Header gone: describe returns not found.
	_, err = svc.DescribeLaunchTemplates(context.Background(), &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateIds: []*string{aws.String(id)},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdNotFound, err.Error())

	// Version bodies and name index gone.
	nums, err := svc.listVersionNumbers(testAccountID, id)
	require.NoError(t, err)
	assert.Empty(t, nums)
	_, err = svc.kv.Get(nameKey(testAccountID, "web"))
	require.Error(t, err)

	// Name is reusable after delete.
	createTemplate(t, svc, "web", "t3.large")
}

func TestDeleteLaunchTemplate_Unknown(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{
		LaunchTemplateId: aws.String("lt-doesnotexist000"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdNotFound, err.Error())
}

func TestResolveHeader_IdNameConflict(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")
	_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{
		LaunchTemplateId:   lt.LaunchTemplateId,
		LaunchTemplateName: aws.String("web"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}
