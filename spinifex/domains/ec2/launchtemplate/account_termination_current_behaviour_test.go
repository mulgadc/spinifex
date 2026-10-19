package launchtemplate

//test:in-package — the faults are injected by wrapping the service's KV
// handle, which is unexported, and the three record shapes under test are private.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_AccountTermination_ tests pin how launch templates
// are stored and what Describe leaves behind today; a failure means it
// changed. The TestAccountTermination_ tests pin the corrected owner delete.

var errInjected = errors.New("injected store failure")

// faultyKV fails the operations its predicates select and passes the rest to
// a real bucket, standing in for a partial KV outage.
type faultyKV struct {
	jetstream.KeyValue

	failGet      func(key string) bool
	failPut      func(key string) bool
	failDelete   func(key string) bool
	failKeys     func() bool
	failUpdate   func(key string) bool
	beforeDelete func(key string)
	beforeCreate func(key string)
	beforeUpdate func(key string)
}

func (k *faultyKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if k.beforeCreate != nil {
		k.beforeCreate(key)
	}
	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k *faultyKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if k.beforeUpdate != nil {
		k.beforeUpdate(key)
	}
	if k.failUpdate != nil && k.failUpdate(key) {
		return 0, errInjected
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}

func (k *faultyKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if k.failGet != nil && k.failGet(key) {
		return nil, errInjected
	}
	return k.KeyValue.Get(ctx, key)
}

func (k *faultyKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if k.failPut != nil && k.failPut(key) {
		return 0, errInjected
	}
	return k.KeyValue.Put(ctx, key, value)
}

func (k *faultyKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if k.beforeDelete != nil {
		k.beforeDelete(key)
	}
	if k.failDelete != nil && k.failDelete(key) {
		return errInjected
	}
	return k.KeyValue.Delete(ctx, key, opts...)
}

func (k *faultyKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if k.failKeys != nil && k.failKeys() {
		return nil, errInjected
	}
	return k.KeyValue.Keys(ctx, opts...)
}

const ctUserData = "#!/bin/sh\necho tenant-secret\n"

// createWithUserData creates a template whose version 1 carries user data,
// then adds version 2.
func createWithUserData(t *testing.T, svc *LaunchTemplateServiceImpl, name string) string {
	t.Helper()
	out, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(name),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:  aws.String("ami-123"),
			UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(ctUserData))),
		},
	}, testAccountID)
	require.NoError(t, err)
	id := aws.StringValue(out.LaunchTemplate.LaunchTemplateId)
	addVersion(t, svc, id, "t3.large", "1")
	return id
}

func storedKeys(t *testing.T, kv jetstream.KeyValue) []string {
	t.Helper()
	keys, err := kv.Keys(t.Context())
	require.NoError(t, err)
	slices.Sort(keys)
	return keys
}

func sortedKeys(keys ...string) []string {
	slices.Sort(keys)
	return keys
}

func storedUserData(t *testing.T, kv jetstream.KeyValue, key string) string {
	t.Helper()
	entry, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	var rec LaunchTemplateVersionRec
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	require.NotNil(t, rec.Data)
	decoded, err := base64.StdEncoding.DecodeString(aws.StringValue(rec.Data.UserData))
	require.NoError(t, err)
	return string(decoded)
}

func describedTemplateIDs(t *testing.T, svc *LaunchTemplateServiceImpl, account string) []string {
	t.Helper()
	out, err := svc.DescribeLaunchTemplates(context.Background(), &ec2.DescribeLaunchTemplatesInput{}, account)
	require.NoError(t, err)
	var ids []string
	for _, lt := range out.LaunchTemplates {
		ids = append(ids, aws.StringValue(lt.LaunchTemplateId))
	}
	return ids
}

// A template is a header <account>.lt-<id>, a name index <account>.name.<hex>
// holding the ID, and one body <account>.lt-<id>.v<n> per version. Bodies
// carry the full LaunchTemplateData, user data included.
func TestCurrentBehaviour_AccountTermination_LaunchTemplateStorageLayout(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web(1)")

	header := testAccountID + "." + id
	name := testAccountID + ".name.776562283129" // hex of "web(1)"
	assert.Equal(t, sortedKeys(header, header+".v1", header+".v2", name, kvutil.VersionKey), storedKeys(t, svc.kv))

	entry, err := svc.kv.Get(t.Context(), name)
	require.NoError(t, err)
	assert.Equal(t, id, string(entry.Value()))
	assert.Equal(t, ctUserData, storedUserData(t, svc.kv, header+".v1"))
	assert.Equal(t, ctUserData, storedUserData(t, svc.kv, header+".v2"), "a version copied from v1 keeps its user data")
}

// The header is deleted last, so a failed body, listing, name-index or header
// delete returns an error with the template still described, and a retried
// delete by ID removes every record, user data included.
func TestAccountTermination_DeleteLaunchTemplateKeepsTheTemplateWhenAStepFails(t *testing.T) {
	tests := []struct {
		name   string
		inject func(kv *faultyKV, header, nameIdx string)
		left   func(header, nameIdx string) []string
	}{
		{
			name: "version body deletes fail",
			inject: func(kv *faultyKV, header, _ string) {
				kv.failDelete = func(key string) bool { return key == header+".v1" || key == header+".v2" }
			},
			left: func(header, nameIdx string) []string {
				return []string{header, header + ".v1", header + ".v2", nameIdx}
			},
		},
		{
			name:   "version listing fails",
			inject: func(kv *faultyKV, _, _ string) { kv.failKeys = func() bool { return true } },
			left: func(header, nameIdx string) []string {
				return []string{header, header + ".v1", header + ".v2", nameIdx}
			},
		},
		{
			name: "name index delete fails",
			inject: func(kv *faultyKV, _, nameIdx string) {
				kv.failDelete = func(key string) bool { return key == nameIdx }
			},
			left: func(header, nameIdx string) []string { return []string{header, nameIdx} },
		},
		{
			name: "header delete fails",
			inject: func(kv *faultyKV, header, _ string) {
				kv.failDelete = func(key string) bool { return key == header }
			},
			left: func(header, _ string) []string { return []string{header} },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := setupTestService(t)
			id := createWithUserData(t, svc, "web")
			inner := svc.kv
			header, nameIdx := testAccountID+"."+id, nameKey(testAccountID, "web")
			faulty := &faultyKV{KeyValue: inner}
			tc.inject(faulty, header, nameIdx)
			svc.kv = faulty

			_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)}, testAccountID)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

			svc.kv = inner
			assert.Equal(t, sortedKeys(append(tc.left(header, nameIdx), kvutil.VersionKey)...), storedKeys(t, inner))
			assert.Equal(t, []string{id}, describedTemplateIDs(t, svc, testAccountID))

			out, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)}, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, id, aws.StringValue(out.LaunchTemplate.LaunchTemplateId))
			assert.Equal(t, []string{kvutil.VersionKey}, storedKeys(t, inner))
			assert.Empty(t, describedTemplateIDs(t, svc, testAccountID))
		})
	}
}

// A header rewritten and a version added while the delete runs send it back
// through the bodies at the new revision, and the late body is removed too.
func TestAccountTermination_DeleteLaunchTemplateRestartsWhenTheHeaderChanges(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	inner := svc.kv
	header := testAccountID + "." + id
	headerEntry, err := inner.Get(t.Context(), header)
	require.NoError(t, err)

	raced := false
	svc.kv = &faultyKV{KeyValue: inner, beforeDelete: func(key string) {
		if key != header || raced {
			return
		}
		raced = true
		_, err := inner.Put(t.Context(), header, headerEntry.Value())
		require.NoError(t, err)
		_, err = inner.Put(t.Context(), header+".v3", []byte(`{"version_number":3}`))
		require.NoError(t, err)
	}}

	_, err = svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)}, testAccountID)
	require.NoError(t, err)
	assert.True(t, raced)
	assert.Equal(t, []string{kvutil.VersionKey}, storedKeys(t, inner))
}

// A create that fails after claiming the name removes what it wrote, so no
// body is orphaned and a retry under the same name succeeds.
func TestAccountTermination_FailedCreateRemovesWhatItWrote(t *testing.T) {
	for _, failing := range []string{"version", "header"} {
		t.Run(failing+" write fails", func(t *testing.T) {
			svc := setupTestService(t)
			inner := svc.kv
			svc.kv = &faultyKV{KeyValue: inner, failPut: func(key string) bool {
				if failing == "header" {
					return isHeaderKey(key, testAccountID+".")
				}
				return versionBodyTemplateID(strings.TrimPrefix(key, testAccountID+".")) != ""
			}}

			_, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
				LaunchTemplateName: aws.String("web"),
				LaunchTemplateData: &ec2.RequestLaunchTemplateData{
					UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(ctUserData))),
				},
			}, testAccountID)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

			svc.kv = inner
			assert.Equal(t, []string{kvutil.VersionKey}, storedKeys(t, inner))
			retried := createTemplate(t, svc, "web", "t3.micro")
			assert.Equal(t, []string{aws.StringValue(retried.LaunchTemplateId)}, describedTemplateIDs(t, svc, testAccountID))
		})
	}
}

// Describe skips a header it cannot read or decode, and still succeeds. Such
// a header cannot be removed through DeleteLaunchTemplate either, and a read
// failure is an internal error, never a missing template.
func TestCurrentBehaviour_AccountTermination_DescribeLaunchTemplatesHidesUnreadableHeaders(t *testing.T) {
	const brokenID = "lt-0badbadbadbadbad0"
	brokenHeader := kvutil.AccountKey(testAccountID, brokenID)
	tests := []struct {
		name       string
		seed       func(t *testing.T, svc *LaunchTemplateServiceImpl)
		deleteCode string
	}{
		{"undecodable header", func(t *testing.T, svc *LaunchTemplateServiceImpl) {
			_, err := svc.kv.Put(t.Context(), brokenHeader, []byte("{not json"))
			require.NoError(t, err)
		}, awserrors.ErrorServerInternal},
		{"header the store cannot read", func(t *testing.T, svc *LaunchTemplateServiceImpl) {
			data, err := json.Marshal(LaunchTemplateHeader{LaunchTemplateId: brokenID, LaunchTemplateName: "hidden", AccountID: testAccountID})
			require.NoError(t, err)
			_, err = svc.kv.Put(t.Context(), brokenHeader, data)
			require.NoError(t, err)
			svc.kv = &faultyKV{KeyValue: svc.kv, failGet: func(key string) bool { return key == brokenHeader }}
		}, awserrors.ErrorServerInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := setupTestService(t)
			live := createTemplate(t, svc, "live", "t3.micro")
			tc.seed(t, svc)

			assert.Contains(t, storedKeys(t, svc.kv), brokenHeader)
			assert.Equal(t, []string{aws.StringValue(live.LaunchTemplateId)}, describedTemplateIDs(t, svc, testAccountID))

			_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(brokenID)}, testAccountID)
			require.Error(t, err)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tc.deleteCode, code)
			assert.Contains(t, storedKeys(t, svc.kv), brokenHeader)
		})
	}
}

// Account cleanup never deletes a live template: every key of a readable
// template stays and its ID is reported kept. Headerless bodies and names are
// removed once settled; a record younger than the settle window, and an
// unreadable header with its bodies and name, are kept.
func TestAccountCleanup_KeepsTemplatesAndRemovesSettledResidue(t *testing.T) {
	svc := setupTestService(t)
	live := createWithUserData(t, svc, "web")
	liveKeys := accountKeys(t, svc.kv, testAccountID)
	other := "999988887777"
	_, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("theirs"), LaunchTemplateData: &ec2.RequestLaunchTemplateData{ImageId: aws.String("ami-123")},
	}, other)
	require.NoError(t, err)
	orphanBody := versionKey(testAccountID, "lt-0orphan000000000", 1)
	orphanName := nameKey(testAccountID, "gone")
	brokenHeader := kvutil.AccountKey(testAccountID, "lt-0broken000000000")
	brokenBody := versionKey(testAccountID, "lt-0broken000000000", 1)
	brokenName := nameKey(testAccountID, "broken")
	for key, value := range map[string]string{
		orphanBody: `{"version_number":1}`, orphanName: "lt-0orphan000000000",
		brokenHeader: "{not json", brokenBody: `{"version_number":1}`, brokenName: "lt-0broken000000000",
	} {
		_, err := svc.kv.Put(t.Context(), key, []byte(value))
		require.NoError(t, err)
	}
	keep := sortedKeys(append(liveKeys, brokenHeader, brokenBody, brokenName)...)

	out, err := svc.CleanupAccountResidue(t.Context(), testAccountID, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, []string{live}, out.KeptTemplateIDs)
	assert.Equal(t, 1, out.OrphanBodiesUnsettled)
	assert.Equal(t, 1, out.OrphanNamesUnsettled)
	assert.Equal(t, sortedKeys(append(keep, orphanBody, orphanName)...), accountKeys(t, svc.kv, testAccountID))

	out, err = svc.CleanupAccountResidue(t.Context(), testAccountID, time.Now().Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, []string{live}, out.KeptTemplateIDs)
	assert.Equal(t, 1, out.OrphanBodiesRemoved)
	assert.Equal(t, 1, out.OrphanNamesRemoved)
	assert.Equal(t, keep, accountKeys(t, svc.kv, testAccountID))
	assert.Equal(t, []string{live}, describedTemplateIDs(t, svc, testAccountID))
	assert.Len(t, describedTemplateIDs(t, svc, other), 1)
}

// A public delete interrupted at its header leaves the template live, so the
// account cleanup keeps it rather than finishing it; the public retry does.
func TestAccountCleanup_InterruptedTemplateDeleteIsLeftToThePublicRetry(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	header := kvutil.AccountKey(testAccountID, id)
	kv := svc.kv
	svc.kv = &faultyKV{KeyValue: kv, failDelete: func(key string) bool { return key == header }}
	_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)}, testAccountID)
	require.Error(t, err)
	svc.kv = kv
	require.Equal(t, []string{header}, accountKeys(t, kv, testAccountID), "only the header, the delete's anchor, remains")

	out, err := svc.CleanupAccountResidue(t.Context(), testAccountID, time.Now().Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, []string{id}, out.KeptTemplateIDs)
	assert.Equal(t, []string{header}, accountKeys(t, kv, testAccountID))

	_, err = svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, accountKeys(t, kv, testAccountID))
}

// A listing failure is returned and nothing is deleted.
func TestAccountCleanup_TemplateListingFailureDeletesNothing(t *testing.T) {
	svc := setupTestService(t)
	id := createWithUserData(t, svc, "web")
	kv := svc.kv
	svc.kv = &faultyKV{KeyValue: kv, failKeys: func() bool { return true }}

	_, err := svc.CleanupAccountResidue(t.Context(), testAccountID, time.Now())
	require.Error(t, err)
	svc.kv = kv
	assert.Equal(t, []string{id}, describedTemplateIDs(t, svc, testAccountID))
}

func accountKeys(t *testing.T, kv jetstream.KeyValue, account string) []string {
	t.Helper()
	var mine []string
	for _, key := range storedKeys(t, kv) {
		if strings.HasPrefix(key, account+".") {
			mine = append(mine, key)
		}
	}
	return mine
}

// A settled orphan the store cannot read or delete is counted failed, never
// removed, and is still there for the next run.
func TestAccountCleanup_OrphanReadOrDeleteFailureIsCountedFailed(t *testing.T) {
	for name, fault := range map[string]func(kv *faultyKV, key string){
		"read":   func(kv *faultyKV, key string) { kv.failGet = func(k string) bool { return k == key } },
		"delete": func(kv *faultyKV, key string) { kv.failDelete = func(k string) bool { return k == key } },
	} {
		t.Run(name, func(t *testing.T) {
			svc := setupTestService(t)
			body := versionKey(testAccountID, "lt-0orphan000000000", 1)
			nameIdx := nameKey(testAccountID, "gone")
			for key, value := range map[string]string{body: `{"version_number":1}`, nameIdx: "lt-0orphan000000000"} {
				_, err := svc.kv.Put(t.Context(), key, []byte(value))
				require.NoError(t, err)
			}
			inner := svc.kv
			faulty := &faultyKV{KeyValue: inner}
			fault(faulty, body)
			svc.kv = faulty

			out, err := svc.CleanupAccountResidue(t.Context(), testAccountID, time.Now().Add(time.Second))
			require.NoError(t, err)
			assert.Equal(t, 1, out.OrphanBodiesFailed)
			assert.Zero(t, out.OrphanBodiesRemoved)
			assert.Equal(t, 1, out.OrphanNamesRemoved, "a fault on one record does not stop the rest")
			assert.Equal(t, []string{body}, accountKeys(t, inner, testAccountID))
		})
	}
}
