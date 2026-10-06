package launchtemplate

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
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
	svc, err := NewLaunchTemplateServiceImplWithNATS(t.Context(), nil, nc)
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
	assertAWSError(t, err, awserrors.ErrorInvalidLaunchTemplateNameAlreadyExistsException, "Launch template name already in use.")
}

func TestLaunchTemplateNameWithKVUnsafeCharacters(t *testing.T) {
	for _, name := range []string{"foo(bar)", "foo."} {
		t.Run(name, func(t *testing.T) {
			svc := setupTestService(t)
			createTemplate(t, svc, name, "t3.micro")

			// Name-based operations must use the same encoded index key.
			_, err := svc.CreateLaunchTemplateVersion(context.Background(), &ec2.CreateLaunchTemplateVersionInput{
				LaunchTemplateName: aws.String(name),
				LaunchTemplateData: &ec2.RequestLaunchTemplateData{InstanceType: aws.String("t3.large")},
			}, testAccountID)
			require.NoError(t, err)

			_, err = svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{
				LaunchTemplateName: aws.String(name),
			}, testAccountID)
			require.NoError(t, err)
		})
	}
}

// TestCreateLaunchTemplate_OrphanNameReclaim verifies repair-on-write: a name
// whose header is gone (crash orphan) is reclaimed by the next create.
func TestCreateLaunchTemplate_OrphanNameReclaim(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "orphan", "t3.micro")

	// Simulate a crash after the name claim but with the header lost.
	require.NoError(t, svc.kv.Delete(t.Context(), kvutil.AccountKey(testAccountID, aws.StringValue(lt.LaunchTemplateId))))

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
	assertAWSError(t, err, awserrors.ErrorInvalidLaunchTemplateNameNotFoundException,
		"At least one of the launch templates specified in the request does not exist.")
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
	assertAWSError(t, err, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound,
		"Could not find the specified version 99 for the launch template with ID "+aws.StringValue(lt.LaunchTemplateId)+".")
}

func TestLaunchTemplate_NotFoundMessagesNameTheTemplate(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro")

	_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{
		LaunchTemplateName: aws.String("missing"),
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorInvalidLaunchTemplateNameNotFoundException,
		"The specified launch template, with template name missing, does not exist.")

	_, err = svc.DescribeLaunchTemplateVersions(context.Background(), &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String("lt-0123456789abcdef0"),
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorInvalidLaunchTemplateIdNotFound,
		"The specified launch template, with template ID lt-0123456789abcdef0, does not exist.")

	_, err = svc.ModifyLaunchTemplate(context.Background(), &ec2.ModifyLaunchTemplateInput{
		LaunchTemplateId: lt.LaunchTemplateId,
		DefaultVersion:   aws.String("42"),
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound,
		"The launch template version 42 is not found for the specified launch template.")

	_, err = svc.CreateLaunchTemplateVersion(context.Background(), &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId:   lt.LaunchTemplateId,
		SourceVersion:      aws.String("42"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{InstanceType: aws.String("t3.small")},
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, "Launch template version does not exist.")
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
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, code)
}

// --- DeleteLaunchTemplateVersions ---

func TestDeleteLaunchTemplateVersions_RejectDefault(t *testing.T) {
	svc := setupTestService(t)
	lt := createTemplate(t, svc, "web", "t3.micro") // default = v1
	id := aws.StringValue(lt.LaunchTemplateId)
	addVersion(t, svc, id, "a", "") // v2

	// The default alongside a deletable version still fails the whole request.
	_, err := svc.DeleteLaunchTemplateVersions(context.Background(), &ec2.DeleteLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id),
		Versions:         []*string{aws.String("2"), aws.String("1")},
	}, testAccountID)
	require.Error(t, err)
	code, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
	assert.Equal(t, "The default version cannot be deleted. Either specify another version as default or delete the launch template.", msg)

	nums, err := svc.listVersionNumbers(t.Context(), testAccountID, id)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{1, 2}, nums)
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
	missing := out.UnsuccessfullyDeletedLaunchTemplateVersions[0]
	assert.Equal(t, int64(9), aws.Int64Value(missing.VersionNumber))
	assert.Equal(t, "launchTemplateVersionDoesNotExist", aws.StringValue(missing.ResponseError.Code))
	assert.Equal(t, "The launch template version does not exist.", aws.StringValue(missing.ResponseError.Message))
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
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdNotFound, code)

	// Version bodies and name index gone.
	nums, err := svc.listVersionNumbers(t.Context(), testAccountID, id)
	require.NoError(t, err)
	assert.Empty(t, nums)
	_, err = svc.kv.Get(t.Context(), nameKey(testAccountID, "web"))
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
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdNotFound, code)
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

func TestResolveHeader_MalformedId(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{
		LaunchTemplateId: aws.String("not-an-lt-id"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdMalformed, err.Error())
}

func TestDescribeLaunchTemplates_MalformedId(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DescribeLaunchTemplates(context.Background(), &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateIds: []*string{aws.String("bad-id")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdMalformed, err.Error())
}

// assertAWSError checks the code a client receives and the message with it.
func assertAWSError(t *testing.T, err error, wantCode, wantMessage string) {
	t.Helper()
	require.Error(t, err)
	code, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "unresolvable error: %v", err)
	assert.Equal(t, wantCode, code)
	assert.Equal(t, wantMessage, msg)
}
