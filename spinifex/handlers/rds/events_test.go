package handlers_rds

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Writes an event dated at when, which the append path cannot do on its own —
// it always stamps "now".
func seedEvent(t *testing.T, svc *Service, sourceType, sourceID string, when time.Time, message string, categories ...string) {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	key := EventRingKey(sourceType, sourceID)

	var ring eventRing
	_, err = getJSON(t.Context(), kv, key, &ring)
	require.NoError(t, err)
	ring.Events = append(ring.Events, Event{
		SourceIdentifier: sourceID,
		SourceType:       sourceType,
		Message:          message,
		Categories:       categories,
		Date:             when,
	})
	require.NoError(t, putJSON(t.Context(), kv, key, &ring))
}

func describeEvents(t *testing.T, svc *Service, input *rds.DescribeEventsInput) []*rds.Event {
	t.Helper()
	out, err := svc.DescribeEvents(t.Context(), input, testAccountID)
	require.NoError(t, err)
	return out.Events
}

func TestDescribeEvents_ReturnsARecordedEventWithItsSourceARN(t *testing.T) {
	svc := newTestService(t)
	svc.RecordEvent(context.Background(), testAccountID, EventSourceTypeDBInstance, testDBID,
		"DB instance stopped.", EventCategoryAvailability)

	events := describeEvents(t, svc, &rds.DescribeEventsInput{})
	require.Len(t, events, 1)
	assert.Equal(t, testDBID, aws.StringValue(events[0].SourceIdentifier))
	assert.Equal(t, EventSourceTypeDBInstance, aws.StringValue(events[0].SourceType))
	assert.Equal(t, "DB instance stopped.", aws.StringValue(events[0].Message))
	assert.Equal(t, []string{EventCategoryAvailability}, aws.StringValueSlice(events[0].EventCategories))
	assert.Equal(t, DBInstanceARN(testRegion, testAccountID, testDBID), aws.StringValue(events[0].SourceArn))
}

// AWS's default window is one hour, so an event outside it is not returned even
// though it is still inside the retention window.
func TestDescribeEvents_ScopesToTheRequestedWindow(t *testing.T) {
	svc := newTestService(t)
	now := time.Now().UTC()
	seedEvent(t, svc, EventSourceTypeDBInstance, testDBID, now.Add(-10*time.Minute), "recent")
	seedEvent(t, svc, EventSourceTypeDBInstance, testDBID, now.Add(-6*time.Hour), "older")

	assert.Equal(t, []string{"recent"}, messagesOf(describeEvents(t, svc, &rds.DescribeEventsInput{})))

	widened := describeEvents(t, svc, &rds.DescribeEventsInput{Duration: aws.Int64(720)})
	assert.Equal(t, []string{"older", "recent"}, messagesOf(widened), "oldest first, as AWS returns them")
}

func TestDescribeEvents_FiltersByCategory(t *testing.T) {
	svc := newTestService(t)
	now := time.Now().UTC()
	seedEvent(t, svc, EventSourceTypeDBInstance, testDBID, now.Add(-time.Minute), "backed up", EventCategoryBackup)
	seedEvent(t, svc, EventSourceTypeDBInstance, testDBID, now, "restarted", EventCategoryAvailability)

	filtered := describeEvents(t, svc, &rds.DescribeEventsInput{
		EventCategories: aws.StringSlice([]string{EventCategoryBackup}),
	})
	assert.Equal(t, []string{"backed up"}, messagesOf(filtered))
}

// An unfiltered read is account-wide, so a resource's history is reachable even
// after the resource itself is gone.
func TestDescribeEvents_ReadsAcrossResourcesAndScopesToOneWhenAsked(t *testing.T) {
	svc := newTestService(t)
	now := time.Now().UTC()
	seedEvent(t, svc, EventSourceTypeDBInstance, testDBID, now, "instance event")
	seedEvent(t, svc, EventSourceTypeDBSnapshot, "orders-db-final", now, "snapshot event")

	assert.ElementsMatch(t, []string{"instance event", "snapshot event"},
		messagesOf(describeEvents(t, svc, &rds.DescribeEventsInput{})))

	scoped := describeEvents(t, svc, &rds.DescribeEventsInput{
		SourceType:       aws.String(EventSourceTypeDBSnapshot),
		SourceIdentifier: aws.String("orders-db-final"),
	})
	assert.Equal(t, []string{"snapshot event"}, messagesOf(scoped))
}

func TestDescribeEvents_RejectsMalformedRequests(t *testing.T) {
	cases := []struct {
		name    string
		input   *rds.DescribeEventsInput
		wantErr string
	}{
		// Two resource kinds may share a name, so an identifier alone is
		// ambiguous — AWS's own rule.
		{"IdentifierWithoutType", &rds.DescribeEventsInput{
			SourceIdentifier: aws.String(testDBID),
		}, awserrors.ErrorInvalidParameterCombination},
		{"UnknownSourceType", &rds.DescribeEventsInput{
			SourceType: aws.String("db-cluster"),
		}, awserrors.ErrorInvalidParameterValue},
		{"EndBeforeStart", &rds.DescribeEventsInput{
			StartTime: aws.Time(time.Now()),
			EndTime:   aws.Time(time.Now().Add(-time.Hour)),
		}, awserrors.ErrorInvalidParameterCombination},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			_, err := svc.DescribeEvents(t.Context(), tc.input, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// The ring is bounded, so a resource's whole history stays one KV value however
// long it lives.
func TestAppendEvent_BoundsTheRing(t *testing.T) {
	svc := newTestService(t)
	for range eventRingSize + 10 {
		require.NoError(t, svc.appendEvent(context.Background(), testAccountID,
			EventSourceTypeDBInstance, testDBID, "beat", nil))
	}

	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var ring eventRing
	found, err := getJSON(t.Context(), kv, EventRingKey(EventSourceTypeDBInstance, testDBID), &ring)
	require.NoError(t, err)
	require.True(t, found)
	assert.Len(t, ring.Events, eventRingSize)
}

// AWS's 14-day retention: anything older is dropped on the next append and
// never returned by a read.
func TestAppendEvent_DropsEventsPastTheRetentionWindow(t *testing.T) {
	svc := newTestService(t)
	seedEvent(t, svc, EventSourceTypeDBInstance, testDBID,
		time.Now().UTC().Add(-EventRetention-time.Hour), "ancient")

	require.NoError(t, svc.appendEvent(context.Background(), testAccountID,
		EventSourceTypeDBInstance, testDBID, "fresh", nil))

	events := describeEvents(t, svc, &rds.DescribeEventsInput{Duration: aws.Int64(60 * 24 * 30)})
	assert.Equal(t, []string{"fresh"}, messagesOf(events))
}

// An event narrates work that already happened, so a failed append must not
// turn a successful operation into a failed one.
func TestRecordEvent_DoesNotPropagateAFailure(t *testing.T) {
	svc := NewService(nil, testRegion)
	assert.NotPanics(t, func() {
		svc.RecordEvent(context.Background(), testAccountID, EventSourceTypeDBInstance, testDBID, "stopped")
	})
}

func messagesOf(events []*rds.Event) []string {
	messages := make([]string, 0, len(events))
	for _, event := range events {
		messages = append(messages, aws.StringValue(event.Message))
	}
	return messages
}
