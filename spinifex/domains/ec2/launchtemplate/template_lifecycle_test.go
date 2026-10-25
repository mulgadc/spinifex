package launchtemplate

//test:in-package — the interleavings are forced by wrapping the service's
// unexported KV handle.

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// raceDelete runs a whole DeleteLaunchTemplate on the unwrapped bucket, so the
// hook that calls it is not re-entered.
func raceDelete(t *testing.T, svc *LaunchTemplateServiceImpl, id string) {
	t.Helper()
	other := &LaunchTemplateServiceImpl{kv: svc.kv.(*faultyKV).KeyValue}
	_, err := other.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)}, testAccountID)
	require.NoError(t, err)
}

func newVersionInput(id string) *ec2.CreateLaunchTemplateVersionInput {
	return &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId:   aws.String(id),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{InstanceType: aws.String("t3.small")},
	}
}

// A version create paused before it writes its body or before it commits the
// header, while a whole delete runs, fails NotFound and leaves nothing behind.
func TestAccountTermination_VersionCreateRacingADeleteLeavesNoBody(t *testing.T) {
	for _, pause := range []string{"before-body", "before-commit"} {
		t.Run(pause, func(t *testing.T) {
			svc := setupTestService(t)
			id := createWithUserData(t, svc, "web")
			header := kvutil.AccountKey(testAccountID, id)
			raced := false
			race := func(key string, at string) {
				if raced || pause != at {
					return
				}
				if (at == "before-body" && key == versionKey(testAccountID, id, 3)) || (at == "before-commit" && key == header) {
					raced = true
					raceDelete(t, svc, id)
				}
			}
			svc.kv = &faultyKV{
				KeyValue:     svc.kv,
				beforeCreate: func(key string) { race(key, "before-body") },
				beforeUpdate: func(key string) { race(key, "before-commit") },
			}

			_, err := svc.CreateLaunchTemplateVersion(context.Background(), newVersionInput(id), testAccountID)
			require.True(t, raced)
			require.Error(t, err)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdNotFound, code)
			assert.Empty(t, accountKeys(t, svc.kv, testAccountID), "no headerless body is left")
		})
	}
}

// A version that commits before the delete removes the header moves the
// header revision, so the delete restarts and removes the new body too.
func TestAccountTermination_DeleteRacingACommittedVersionRemovesIt(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	inner := svc.kv
	header := kvutil.AccountKey(testAccountID, id)
	raced := false
	svc.kv = &faultyKV{KeyValue: inner, beforeDelete: func(key string) {
		if key != header || raced {
			return
		}
		raced = true
		other := &LaunchTemplateServiceImpl{kv: inner}
		_, err := other.CreateLaunchTemplateVersion(context.Background(), newVersionInput(id), testAccountID)
		require.NoError(t, err)
	}}

	_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)}, testAccountID)
	require.NoError(t, err)
	assert.True(t, raced)
	assert.Empty(t, accountKeys(t, inner, testAccountID))
}

// A header commit that fails removes the body it guarded and returns an
// error; the template keeps exactly the versions it had.
func TestAccountTermination_FailedVersionCommitRemovesTheBody(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	before := accountKeys(t, svc.kv, testAccountID)
	header := kvutil.AccountKey(testAccountID, id)
	svc.kv = &faultyKV{KeyValue: svc.kv, failUpdate: func(key string) bool { return key == header }}

	_, err := svc.CreateLaunchTemplateVersion(context.Background(), newVersionInput(id), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Equal(t, before, accountKeys(t, svc.kv, testAccountID))
}

// A version create racing a default-version change commits at the new header
// revision and keeps the change.
func TestAccountTermination_VersionCommitKeepsAConcurrentHeaderChange(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	inner := svc.kv
	header := kvutil.AccountKey(testAccountID, id)
	raced := false
	svc.kv = &faultyKV{KeyValue: inner, beforeUpdate: func(key string) {
		if key != header || raced {
			return
		}
		raced = true
		other := &LaunchTemplateServiceImpl{kv: inner}
		_, err := other.ModifyLaunchTemplate(context.Background(), &ec2.ModifyLaunchTemplateInput{
			LaunchTemplateId: aws.String(id), DefaultVersion: aws.String("2"),
		}, testAccountID)
		require.NoError(t, err)
	}}

	out, err := svc.CreateLaunchTemplateVersion(context.Background(), newVersionInput(id), testAccountID)
	require.NoError(t, err)
	assert.True(t, raced)
	assert.Equal(t, int64(3), aws.Int64Value(out.LaunchTemplateVersion.VersionNumber))
	h, _, err := svc.getHeaderByID(context.Background(), testAccountID, id)
	require.NoError(t, err)
	assert.Equal(t, int64(2), h.DefaultVersionNumber)
}

// Template data carrying a key name and a placement group reaches the
// RunInstances input for $Default, $Latest and an explicit version.
func TestCurrentBehaviour_AccountTermination_TemplateKeyAndGroupReachRunInstances(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId: aws.String("ami-123"), KeyName: aws.String("v1-key"),
			Placement: &ec2.LaunchTemplatePlacementRequest{GroupName: aws.String("v1-group")},
		},
	}, testAccountID)
	require.NoError(t, err)
	id := aws.StringValue(out.LaunchTemplate.LaunchTemplateId)
	_, err = svc.CreateLaunchTemplateVersion(context.Background(), &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: aws.String(id),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId: aws.String("ami-123"), KeyName: aws.String("v2-key"),
			Placement: &ec2.LaunchTemplatePlacementRequest{GroupName: aws.String("v2-group")},
		},
	}, testAccountID)
	require.NoError(t, err)

	for version, want := range map[string]string{"$Default": "v1", "$Latest": "v2", "2": "v2", "1": "v1"} {
		in := &ec2.RunInstancesInput{LaunchTemplate: &ec2.LaunchTemplateSpecification{LaunchTemplateId: aws.String(id), Version: aws.String(version)}}
		require.NoError(t, ExpandRunInstances(context.Background(), svc, in, testAccountID), version)
		assert.Equal(t, want+"-key", aws.StringValue(in.KeyName), version)
		require.NotNil(t, in.Placement, version)
		assert.Equal(t, want+"-group", aws.StringValue(in.Placement.GroupName), version)
	}
}

// The reference reader returns what every version of every live template
// names, and nothing from headerless bodies or other accounts.
func TestAccountTemplateReferences_ReadsEveryVersionOfLiveTemplates(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{KeyName: aws.String("default-key")},
	}, testAccountID)
	require.NoError(t, err)
	id := aws.StringValue(out.LaunchTemplate.LaunchTemplateId)
	_, err = svc.CreateLaunchTemplateVersion(context.Background(), &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: aws.String(id),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			KeyName:   aws.String("later-key"),
			Placement: &ec2.LaunchTemplatePlacementRequest{GroupName: aws.String("later-group"), GroupId: aws.String("pg-0123456789abcdef0")},
		},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("theirs"), LaunchTemplateData: &ec2.RequestLaunchTemplateData{KeyName: aws.String("their-key")},
	}, "999988887777")
	require.NoError(t, err)
	_, err = svc.kv.Put(t.Context(), versionKey(testAccountID, "lt-0orphan000000000", 1), []byte(`{"version_number":1,"data":{"KeyName":"orphan-key"}}`))
	require.NoError(t, err)

	refs, err := svc.AccountTemplateReferences(t.Context(), testAccountID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"default-key", "later-key"}, refs.KeyNames)
	assert.Equal(t, []string{"later-group"}, refs.PlacementGroupNames)
	assert.Equal(t, []string{"pg-0123456789abcdef0"}, refs.PlacementGroupIDs)
}

// A header or version the reader cannot read or decode, or a listing
// failure, is an error: the unread record may name the resource.
func TestAccountTemplateReferences_FailsClosed(t *testing.T) {
	tests := map[string]func(t *testing.T, svc *LaunchTemplateServiceImpl, id string){
		"listing fails": func(t *testing.T, svc *LaunchTemplateServiceImpl, _ string) {
			svc.kv = &faultyKV{KeyValue: svc.kv, failKeys: func() bool { return true }}
		},
		"version unreadable": func(t *testing.T, svc *LaunchTemplateServiceImpl, id string) {
			svc.kv = &faultyKV{KeyValue: svc.kv, failGet: func(key string) bool { return key == versionKey(testAccountID, id, 2) }}
		},
		"version undecodable": func(t *testing.T, svc *LaunchTemplateServiceImpl, id string) {
			_, err := svc.kv.Put(t.Context(), versionKey(testAccountID, id, 2), []byte("{not json"))
			require.NoError(t, err)
		},
		"version without data": func(t *testing.T, svc *LaunchTemplateServiceImpl, id string) {
			_, err := svc.kv.Put(t.Context(), versionKey(testAccountID, id, 2), []byte(`{"version_number":2}`))
			require.NoError(t, err)
		},
		"header unreadable": func(t *testing.T, svc *LaunchTemplateServiceImpl, id string) {
			svc.kv = &faultyKV{KeyValue: svc.kv, failGet: func(key string) bool { return key == kvutil.AccountKey(testAccountID, id) }}
		},
		"header undecodable": func(t *testing.T, svc *LaunchTemplateServiceImpl, _ string) {
			_, err := svc.kv.Put(t.Context(), kvutil.AccountKey(testAccountID, "lt-0broken000000000"), []byte("{not json"))
			require.NoError(t, err)
		},
	}
	for name, seed := range tests {
		t.Run(name, func(t *testing.T) {
			svc := setupTestService(t)
			id := createWithUserData(t, svc, "web")
			seed(t, svc, id)
			refs, err := svc.AccountTemplateReferences(t.Context(), testAccountID)
			require.Error(t, err)
			assert.Nil(t, refs)
		})
	}
}

func setDefault(svc *LaunchTemplateServiceImpl, id, version string) error {
	_, err := svc.ModifyLaunchTemplate(context.Background(), &ec2.ModifyLaunchTemplateInput{
		LaunchTemplateId: aws.String(id), DefaultVersion: aws.String(version),
	}, testAccountID)
	return err
}

func deleteVersions(svc *LaunchTemplateServiceImpl, id string, versions ...string) (*ec2.DeleteLaunchTemplateVersionsOutput, error) {
	return svc.DeleteLaunchTemplateVersions(context.Background(), &ec2.DeleteLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(id), Versions: aws.StringSlice(versions),
	}, testAccountID)
}

// requireDefaultResolves asserts the header's default version has a body.
func requireDefaultResolves(t *testing.T, svc *LaunchTemplateServiceImpl, id string, want int64) {
	t.Helper()
	h, _, err := svc.getHeaderByID(context.Background(), testAccountID, id)
	require.NoError(t, err)
	assert.Equal(t, want, h.DefaultVersionNumber)
	_, err = svc.getVersion(context.Background(), testAccountID, id, h.DefaultVersionNumber)
	assert.NoError(t, err, "the default version has a body")
}

// A default change that commits while a version delete is removing the new
// default makes the delete fail as a default-version delete and restore it.
func TestDeleteVersionsRacingADefaultChangeKeepsTheDefault(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	inner := svc.kv
	raced := false
	svc.kv = &faultyKV{KeyValue: inner, beforeDelete: func(key string) {
		if key != versionKey(testAccountID, id, 2) || raced {
			return
		}
		raced = true
		require.NoError(t, setDefault(&LaunchTemplateServiceImpl{kv: inner}, id, "2"))
	}}

	_, err := deleteVersions(svc, id, "2")
	require.True(t, raced)
	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
	requireDefaultResolves(t, svc, id, 2)
}

// A default change paused before its header write, while the target version
// is deleted, fails VersionNotFound and leaves the old default.
func TestDefaultChangeRacingAVersionDeleteFailsNotFound(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	inner := svc.kv
	header := kvutil.AccountKey(testAccountID, id)
	raced := false
	svc.kv = &faultyKV{KeyValue: inner, beforeUpdate: func(key string) {
		if key != header || raced {
			return
		}
		raced = true
		out, err := deleteVersions(&LaunchTemplateServiceImpl{kv: inner}, id, "2")
		require.NoError(t, err)
		require.Len(t, out.SuccessfullyDeletedLaunchTemplateVersions, 1)
	}}

	err := setDefault(svc, id, "2")
	require.True(t, raced)
	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdVersionNotFound, code)
	requireDefaultResolves(t, svc, id, 1)
}

// A default change to a version the delete does not name lets the delete
// commit at the new header revision and keeps the change.
func TestDeleteVersionsKeepsAnUnrelatedDefaultChange(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	addVersion(t, svc, id, "t3.large", "") // v3
	inner := svc.kv
	raced := false
	svc.kv = &faultyKV{KeyValue: inner, beforeDelete: func(key string) {
		if key != versionKey(testAccountID, id, 2) || raced {
			return
		}
		raced = true
		require.NoError(t, setDefault(&LaunchTemplateServiceImpl{kv: inner}, id, "3"))
	}}

	out, err := deleteVersions(svc, id, "2")
	require.NoError(t, err)
	assert.True(t, raced)
	require.Len(t, out.SuccessfullyDeletedLaunchTemplateVersions, 1)
	requireDefaultResolves(t, svc, id, 3)
	_, err = svc.getVersion(context.Background(), testAccountID, id, 2)
	assert.Error(t, err)
}

// A version delete whose header commit fails restores the bodies it removed.
func TestFailedVersionDeleteCommitRestoresTheBodies(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	before := accountKeys(t, svc.kv, testAccountID)
	header := kvutil.AccountKey(testAccountID, id)
	svc.kv = &faultyKV{KeyValue: svc.kv, failUpdate: func(key string) bool { return key == header }}

	_, err := deleteVersions(svc, id, "2")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Equal(t, before, accountKeys(t, svc.kv, testAccountID))
}

// A template deleted while its versions are being deleted fails NotFound and
// leaves nothing behind.
func TestVersionDeleteRacingATemplateDeleteLeavesNothing(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	header := kvutil.AccountKey(testAccountID, id)
	raced := false
	svc.kv = &faultyKV{KeyValue: svc.kv, beforeUpdate: func(key string) {
		if key != header || raced {
			return
		}
		raced = true
		raceDelete(t, svc, id)
	}}

	_, err := deleteVersions(svc, id, "2")
	require.True(t, raced)
	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdNotFound, code)
	assert.Empty(t, accountKeys(t, svc.kv, testAccountID))
}

// A template deleted after a version delete lost to a default change, but
// before it restored the bodies, is not left with restored headerless bodies.
func TestRestoredVersionsAfterATemplateDeleteLeaveNothing(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	inner := svc.kv
	v2 := versionKey(testAccountID, id, 2)
	defaulted, deleted := false, false
	svc.kv = &faultyKV{KeyValue: inner,
		beforeDelete: func(key string) {
			if key != v2 || defaulted {
				return
			}
			defaulted = true
			require.NoError(t, setDefault(&LaunchTemplateServiceImpl{kv: inner}, id, "2"))
		},
		beforeCreate: func(key string) {
			if key != v2 || deleted {
				return
			}
			deleted = true
			raceDelete(t, svc, id)
		},
	}

	_, err := deleteVersions(svc, id, "2")
	require.True(t, deleted)
	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorInvalidLaunchTemplateIdNotFound, code)
	assert.Empty(t, accountKeys(t, inner, testAccountID))
}

// A default change whose header write loses to a version create re-reads the
// header and commits at its new revision.
func TestDefaultChangeRetriesAfterAConcurrentVersionCreate(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	inner := svc.kv
	header := kvutil.AccountKey(testAccountID, id)
	raced := false
	svc.kv = &faultyKV{KeyValue: inner, beforeUpdate: func(key string) {
		if key != header || raced {
			return
		}
		raced = true
		_, err := (&LaunchTemplateServiceImpl{kv: inner}).CreateLaunchTemplateVersion(context.Background(), newVersionInput(id), testAccountID)
		require.NoError(t, err)
	}}

	require.NoError(t, setDefault(svc, id, "2"))
	require.True(t, raced)
	requireDefaultResolves(t, svc, id, 2)
	_, err := svc.getVersion(context.Background(), testAccountID, id, 3)
	assert.NoError(t, err)
}

// A version delete that loses to a default change but cannot restore the
// version fails InternalError, not the refusal that implies nothing was deleted.
func TestUnrestoredVersionDeleteFailsInternalError(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	inner := svc.kv
	v2 := versionKey(testAccountID, id, 2)
	raced := false
	svc.kv = &faultyKV{KeyValue: inner,
		beforeDelete: func(key string) {
			if key != v2 || raced {
				return
			}
			raced = true
			require.NoError(t, setDefault(&LaunchTemplateServiceImpl{kv: inner}, id, "2"))
		},
		failCreate: func(key string) bool { return key == v2 },
	}

	_, err := deleteVersions(svc, id, "2")
	require.True(t, raced)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
