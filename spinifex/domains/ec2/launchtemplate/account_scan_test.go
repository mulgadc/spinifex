package launchtemplate

//test:in-package — the scan is run over a read-only wrapper of the service's
// unexported KV handle, and records are seeded in their private layout.

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit/readonlystore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readRecordingKV records every key read through it.
type readRecordingKV struct {
	jetstream.KeyValue

	gets []string
}

func (k *readRecordingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.gets = append(k.gets, key)
	return k.KeyValue.Get(ctx, key)
}

func putRaw(t *testing.T, kv jetstream.KeyValue, key, value string) {
	t.Helper()
	_, err := kv.Put(t.Context(), key, []byte(value))
	require.NoError(t, err)
}

// Each record left by a failed delete or an interrupted create is counted by
// shape, a live template's bodies and name are not, and version bodies are
// classified from their keys without being read.
func TestScanAccountClassifiesLaunchTemplateRecords(t *testing.T) {
	svc := setupTestService(t)
	live := createWithUserData(t, svc, "live")

	deleted := createWithUserData(t, svc, "deleted")
	inner := svc.kv
	deletedHeader, deletedName := kvutil.AccountKey(testAccountID, deleted), nameKey(testAccountID, "deleted")
	svc.kv = &faultyKV{KeyValue: inner, failDelete: func(key string) bool {
		return strings.HasPrefix(key, deletedHeader+".v") || key == deletedName
	}}
	_, err := svc.DeleteLaunchTemplate(context.Background(), &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(deleted)}, testAccountID)
	require.NoError(t, err)

	svc.kv = &faultyKV{KeyValue: inner, failPut: func(key string) bool { return isHeaderKey(key, testAccountID+".") }}
	_, err = svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("interrupted"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(ctUserData)))},
	}, testAccountID)
	require.Error(t, err)
	svc.kv = inner

	putRaw(t, inner, kvutil.AccountKey(testAccountID, "lt-0badbadbadbadbad0"), "{not json")
	putRaw(t, inner, kvutil.AccountKey(testAccountID, "lt-0badbadbadbadbad0.v1"), `{"version_number":1}`)
	putRaw(t, inner, kvutil.AccountKey(testAccountID, "lt-0mismatch00000000"), `{"launch_template_id":"lt-0other"}`)
	putRaw(t, inner, kvutil.AccountKey(testAccountID, "lt-0unreadable000000"), `{"launch_template_id":"lt-0unreadable000000"}`)
	putRaw(t, inner, nameKey(testAccountID, "unreadable"), "lt-0unreadable000000")
	putRaw(t, inner, kvutil.AccountKey(testAccountID, "lt-0bad.vx"), "{}")

	recorder := &readRecordingKV{KeyValue: readonlystore.NewKV(t, inner)}
	svc.kv = &faultyKV{KeyValue: recorder, failGet: func(key string) bool {
		return key == kvutil.AccountKey(testAccountID, "lt-0unreadable000000")
	}}
	scan, err := svc.ScanAccount(context.Background(), testAccountID)
	require.NoError(t, err)

	assert.Equal(t, []string{live}, scan.LiveTemplateIDs)
	assert.ElementsMatch(t, []string{"lt-0badbadbadbadbad0", "lt-0mismatch00000000", "lt-0unreadable000000"}, scan.UnreadableTemplateIDs)
	assert.Equal(t, 3, scan.OrphanVersionBodies, "two bodies of the deleted template and v1 of the interrupted create")
	assert.Equal(t, 2, scan.OrphanNameIndexEntries, "the deleted and the interrupted template's names")
	assert.Equal(t, 1, scan.UnrecognisedEntries)
	assert.Zero(t, scan.UnreadableNameIndexEntries)
	assert.Equal(t, 1, scan.ReadFailures)

	for _, key := range recorder.gets {
		assert.NotRegexp(t, `\.v[0-9]+$`, key, "the scan read a version body")
	}
}

// A name-index entry the store cannot return is unreadable and a read failure.
func TestScanAccountCountsAnUnreadableNameIndexEntry(t *testing.T) {
	svc := setupTestService(t)
	createTemplate(t, svc, "live", "t3.micro")
	name := nameKey(testAccountID, "live")
	svc.kv = &faultyKV{KeyValue: readonlystore.NewKV(t, svc.kv), failGet: func(key string) bool { return key == name }}

	scan, err := svc.ScanAccount(context.Background(), testAccountID)
	require.NoError(t, err)
	assert.Len(t, scan.LiveTemplateIDs, 1)
	assert.Equal(t, 1, scan.UnreadableNameIndexEntries)
	assert.Equal(t, 1, scan.ReadFailures)
	assert.Zero(t, scan.OrphanNameIndexEntries)
}

// The account prefix ends at its dot, so account 1111 never sees 11112's records.
func TestScanAccountStaysInsideTheLaunchTemplateAccountPrefix(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateLaunchTemplate(context.Background(), &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("neighbour"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{ImageId: aws.String("ami-123")},
	}, "11112")
	require.NoError(t, err)
	svc.kv = readonlystore.NewKV(t, svc.kv)

	scan, err := svc.ScanAccount(context.Background(), "1111")
	require.NoError(t, err)
	assert.Equal(t, &AccountScan{}, scan)

	scan, err = svc.ScanAccount(context.Background(), "11112")
	require.NoError(t, err)
	assert.Len(t, scan.LiveTemplateIDs, 1)
	assert.Zero(t, scan.OrphanVersionBodies+scan.OrphanNameIndexEntries+scan.UnrecognisedEntries)
}

// A listing failure is an error, never an empty scan.
func TestScanAccountReturnsALaunchTemplateListingFailure(t *testing.T) {
	svc := setupTestService(t)
	svc.kv = &faultyKV{KeyValue: svc.kv, failKeys: func() bool { return true }}
	_, err := svc.ScanAccount(context.Background(), testAccountID)
	require.ErrorIs(t, err, errInjected)
}

func TestVersionBodyTemplateID(t *testing.T) {
	for rest, want := range map[string]string{
		"lt-0abc.v1":   "lt-0abc",
		"lt-0abc.v12":  "lt-0abc",
		"lt-0abc.v0":   "",
		"lt-0abc.vx":   "",
		"lt-0abc":      "",
		"name.6f72.v1": "",
		"lt-0a.b.v1":   "",
	} {
		assert.Equal(t, want, versionBodyTemplateID(rest), rest)
	}
}
