package launchtemplate

//test:in-package — the faults are injected by wrapping the service's KV
// handle, which is unexported, and the three record shapes under test are private.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_AccountTermination_ tests pin how launch templates
// are stored and what Describe and Delete leave behind today. They describe
// current behaviour, not the target design; a failure means it changed.

var errInjected = errors.New("injected store failure")

// faultyKV fails the operations its predicates select and passes the rest to
// a real bucket, standing in for a partial KV outage.
type faultyKV struct {
	jetstream.KeyValue

	failGet    func(key string) bool
	failPut    func(key string) bool
	failDelete func(key string) bool
	failKeys   func() bool
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

// Pins current behaviour that differs from AWS: the header delete alone
// decides success, and a failed name-index or version-body cleanup leaves
// those records, user data included, with no header to find them by.
func TestCurrentBehaviour_AccountTermination_DeleteLaunchTemplateLeavesRecordsWhenCleanupFails(t *testing.T) {
	tests := []struct {
		name     string
		inject   func(kv *faultyKV, header, nameIdx string)
		residual func(header, nameIdx string) []string
	}{
		{
			name: "version body deletes fail",
			inject: func(kv *faultyKV, header, _ string) {
				kv.failDelete = func(key string) bool { return key == header+".v1" || key == header+".v2" }
			},
			residual: func(header, _ string) []string { return []string{header + ".v1", header + ".v2"} },
		},
		{
			name: "name index delete fails",
			inject: func(kv *faultyKV, _, nameIdx string) {
				kv.failDelete = func(key string) bool { return key == nameIdx }
			},
			residual: func(_, nameIdx string) []string { return []string{nameIdx} },
		},
		{
			name: "version listing fails after the header delete",
			inject: func(kv *faultyKV, header, _ string) {
				headerGone := false
				kv.failDelete = func(key string) bool {
					headerGone = headerGone || key == header
					return false
				}
				kv.failKeys = func() bool { return headerGone }
			},
			residual: func(header, _ string) []string { return []string{header + ".v1", header + ".v2"} },
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

			out, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)}, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, id, aws.StringValue(out.LaunchTemplate.LaunchTemplateId))

			svc.kv = inner
			want := append(tc.residual(header, nameIdx), kvutil.VersionKey)
			assert.Equal(t, sortedKeys(want...), storedKeys(t, inner))
			assert.Empty(t, describedTemplateIDs(t, svc, testAccountID))
			if slices.Contains(want, header+".v1") {
				assert.Equal(t, ctUserData, storedUserData(t, inner, header+".v1"))
			}
		})
	}
}

// A create interrupted before its header write leaves the name claim and
// version 1 behind. A retry under the same name reclaims the name, and the
// first attempt's body is never reachable again.
func TestCurrentBehaviour_AccountTermination_InterruptedCreateLeavesAnOrphanBody(t *testing.T) {
	svc := setupTestService(t)
	inner := svc.kv
	svc.kv = &faultyKV{KeyValue: inner, failPut: func(key string) bool { return isHeaderKey(key, testAccountID+".") }}

	_, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("web"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(ctUserData))),
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

	svc.kv = inner
	keys := storedKeys(t, inner)
	require.Len(t, keys, 3, "name index, version 1 and the bucket version key")
	nameEntry, err := inner.Get(t.Context(), nameKey(testAccountID, "web"))
	require.NoError(t, err)
	orphanBody := testAccountID + "." + string(nameEntry.Value()) + ".v1"
	assert.Contains(t, keys, orphanBody)
	assert.Equal(t, ctUserData, storedUserData(t, inner, orphanBody))
	assert.Empty(t, describedTemplateIDs(t, svc, testAccountID))

	retried := createTemplate(t, svc, "web", "t3.micro")
	assert.NotEqual(t, string(nameEntry.Value()), aws.StringValue(retried.LaunchTemplateId))
	assert.Contains(t, storedKeys(t, inner), orphanBody)
	assert.Equal(t, []string{aws.StringValue(retried.LaunchTemplateId)}, describedTemplateIDs(t, svc, testAccountID))
}

// Describe skips a header it cannot read or decode, and still succeeds. An
// undecodable header also cannot be removed through DeleteLaunchTemplate.
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
		}, awserrors.ErrorInvalidLaunchTemplateIdNotFound},
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
