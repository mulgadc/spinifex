package ec2v1

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPlatformInventorySubject(t *testing.T) {
	require.Equal(t, "ec2.v1.PlatformInventory", PlatformInventorySubject)
}

func TestPlatformInventoryRequestJSONContract(t *testing.T) {
	data, err := json.Marshal(PlatformInventoryRequest{AccountID: "111122223333", CorrelationRef: "req-1"})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333","correlation_ref":"req-1"}`, string(data))

	data, err = json.Marshal(PlatformInventoryRequest{AccountID: "111122223333"})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333"}`, string(data))
}

func TestPlatformInventoryReplyJSONContract(t *testing.T) {
	start := time.Date(2026, 10, 16, 1, 2, 3, 0, time.UTC)
	reply := PlatformInventoryReply{
		AccountID:      "111122223333",
		CorrelationRef: "req-1",
		Kinds: []PlatformKindReport{
			{Kind: PlatformKindKeyPair, Status: PlatformStatusObservedClean, ScanStartedAt: start, ScanEndedAt: start.Add(time.Second)},
			{Kind: PlatformKindPlacementGroup, Status: PlatformStatusRecordsFound, ScanStartedAt: start, ScanEndedAt: start},
			{Kind: PlatformKindLaunchTemplate, Status: PlatformStatusScanIncomplete, IncompleteReasons: []string{PlatformReasonStoreListFailed}, ScanStartedAt: start, ScanEndedAt: start},
			{Kind: PlatformKindTagIndex, Status: PlatformStatusRecordsFound, ScanStartedAt: start, ScanEndedAt: start},
		},
		Items: []PlatformItem{
			{ResourceID: "pg-0held", Kind: PlatformKindPlacementGroup, Disposition: PlatformDispositionDependencyHeld, MemberInstanceIDs: []string{"i-0a"}},
			{ResourceID: "lt-0gone", Kind: PlatformKindTagIndex, Disposition: PlatformDispositionResidue},
		},
		Counts: []PlatformRecordCount{
			{Kind: PlatformKindKeyPair, Disposition: PlatformDispositionResidue, Class: PlatformClassPublicKeyWithoutMetadata, Count: 2},
		},
	}

	data, err := json.Marshal(reply)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"account_id": "111122223333",
		"correlation_ref": "req-1",
		"kinds": [
			{"kind": "key-pair", "status": "observed-clean-at-scan-time", "scan_started_at": "2026-10-16T01:02:03Z", "scan_ended_at": "2026-10-16T01:02:04Z"},
			{"kind": "placement-group", "status": "records-found", "scan_started_at": "2026-10-16T01:02:03Z", "scan_ended_at": "2026-10-16T01:02:03Z"},
			{"kind": "launch-template", "status": "scan-incomplete", "incomplete_reasons": ["store-list-failed"], "scan_started_at": "2026-10-16T01:02:03Z", "scan_ended_at": "2026-10-16T01:02:03Z"},
			{"kind": "tag-index", "status": "records-found", "scan_started_at": "2026-10-16T01:02:03Z", "scan_ended_at": "2026-10-16T01:02:03Z"}
		],
		"items": [
			{"resource_id": "pg-0held", "kind": "placement-group", "disposition": "dependency-held", "member_instance_ids": ["i-0a"]},
			{"resource_id": "lt-0gone", "kind": "tag-index", "disposition": "residue"}
		],
		"counts": [
			{"kind": "key-pair", "disposition": "residue", "class": "public-key-object-without-metadata", "count": 2}
		]
	}`, string(data))
}

// The wire values of every status, disposition, class and reason.
func TestPlatformInventoryVocabulary(t *testing.T) {
	require.Equal(t, []string{"key-pair", "placement-group", "launch-template", "tag-index"}, PlatformKinds())
	require.Equal(t, []string{"live", "residue", "unreadable", "dependency-held"},
		[]string{PlatformDispositionLive, PlatformDispositionResidue, PlatformDispositionUnreadable, PlatformDispositionDependencyHeld})
	require.Equal(t, []string{
		"public-key-object-without-metadata", "public-key-object-unresolved", "unreadable-record",
		"version-body-without-header", "name-index-without-header", "unrecognised-template-entry",
	}, []string{
		PlatformClassPublicKeyWithoutMetadata, PlatformClassPublicKeyUnresolved, PlatformClassUnreadableRecord,
		PlatformClassVersionBodyWithoutHeader, PlatformClassNameIndexWithoutHeader, PlatformClassUnrecognisedTemplateEntry,
	})
	require.Equal(t, []string{
		"store-list-failed", "record-read-failed", "owner-records-unresolved", "no-responder",
		"timeout", "transport-failed", "error-reply", "invalid-reply", "kind-not-reported",
	}, []string{
		PlatformReasonStoreListFailed, PlatformReasonRecordReadFailed, PlatformReasonOwnerRecordsUnresolved, PlatformReasonNoResponder,
		PlatformReasonTimeout, PlatformReasonTransportFailed, PlatformReasonErrorReply, PlatformReasonInvalidReply, PlatformReasonKindNotReported,
	})
}

// A substituted reply never reports a kind clean.
func TestIncompletePlatformInventoryMarksEveryKindIncomplete(t *testing.T) {
	reply := IncompletePlatformInventory("111122223333", "req-1", PlatformReasonNoResponder)
	require.Equal(t, "111122223333", reply.AccountID)
	require.Equal(t, "req-1", reply.CorrelationRef)
	require.Len(t, reply.Kinds, len(PlatformKinds()))
	for i, kind := range PlatformKinds() {
		require.Equal(t, kind, reply.Kinds[i].Kind)
		require.Equal(t, PlatformStatusScanIncomplete, reply.Kinds[i].Status)
		require.Equal(t, []string{PlatformReasonNoResponder}, reply.Kinds[i].IncompleteReasons)
	}
	require.Empty(t, reply.Items)
	require.Empty(t, reply.Counts)
}
