package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// The event ring is the only channel some facts have to the customer — a backup
// that could not be quiesced, a modify applied hours after it was requested, an
// engine that could not be shut down cleanly. Five phases write to it; this one
// owns the mechanism.
const (
	// AWS's own retention. Anything older is dropped on the next append and
	// never returned by a read.
	EventRetention = 14 * 24 * time.Hour

	// The per-resource bound. Deep enough that a customer polling at AWS's
	// default one-hour window never misses an event, shallow enough that a
	// resource's whole history stays one KV value.
	eventRingSize = 100

	// A resource can be appended to from the API path and the reconciler at
	// once, so the CAS is retried rather than dropped on first contention.
	eventWriteAttempts = 8

	// AWS's default window for the read.
	defaultEventDuration = time.Hour
)

// AWS's SourceType values. Only the ones some phase actually writes are
// defined; an unknown one from a request is rejected rather than matched
// against nothing, which would look like a resource with no history.
const (
	EventSourceTypeDBInstance = "db-instance"
	EventSourceTypeDBSnapshot = "db-snapshot"
)

// AWS's event categories. A customer filters and subscribes on these, so the
// strings are AWS's exactly, spaces included.
const (
	EventCategoryAvailability        = "availability"
	EventCategoryBackup              = "backup"
	EventCategoryConfigurationChange = "configuration change"
	EventCategoryCreation            = "creation"
	EventCategoryDeletion            = "deletion"
	EventCategoryFailure             = "failure"
	EventCategoryMaintenance         = "maintenance"
	EventCategoryNotification        = "notification"
	EventCategoryRecovery            = "recovery"
)

func validEventSourceType(sourceType string) bool {
	switch sourceType {
	case EventSourceTypeDBInstance, EventSourceTypeDBSnapshot:
		return true
	default:
		return false
	}
}

// Event is one entry in a resource's ring.
type Event struct {
	SourceIdentifier string    `json:"sourceIdentifier"`
	SourceType       string    `json:"sourceType"`
	Message          string    `json:"message"`
	Categories       []string  `json:"categories,omitempty"`
	Date             time.Time `json:"date"`
}

// Oldest first, so an append is a tail write and a trim is a head drop.
type eventRing struct {
	Events []Event `json:"events"`
}

// RecordEvent appends an event to the resource's ring. Best-effort by design: failing the operation
// because its narration could not be stored would turn a successful stop into a failed one, so
// failures are logged instead.
func (s *Service) RecordEvent(ctx context.Context, accountID, sourceType, sourceIdentifier, message string, categories ...string) {
	if err := s.appendEvent(ctx, accountID, sourceType, sourceIdentifier, message, categories); err != nil {
		slog.WarnContext(ctx, "rds: recording an event failed",
			"sourceType", sourceType, "sourceId", sourceIdentifier, "message", message, "err", err)
	}
}

func (s *Service) appendEvent(ctx context.Context, accountID, sourceType, sourceIdentifier, message string, categories []string) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	key := EventRingKey(sourceType, sourceIdentifier)
	event := Event{
		SourceIdentifier: sourceIdentifier,
		SourceType:       sourceType,
		Message:          message,
		Categories:       categories,
		Date:             time.Now().UTC(),
	}

	for range eventWriteAttempts {
		var ring eventRing
		rev, found, err := getJSONRevision(ctx, kv, key, &ring)
		if err != nil {
			return err
		}
		ring.Events = trimEvents(append(ring.Events, event))

		if !found {
			if err := createJSON(ctx, kv, key, &ring); err == nil {
				return nil
			} else if !errors.Is(err, kvstore.ErrExists) {
				return err
			}
			// Another writer created the ring first; re-read and append to it.
			continue
		}
		err = updateJSON(ctx, kv, key, rev, &ring)
		if err == nil {
			return nil
		}
		// A revision mismatch is a lost race with another appender, not a
		// duplicate: re-read the ring and append to what they wrote.
		if !errors.Is(err, kvstore.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("rds: event append on %s contended after %d attempts", key, eventWriteAttempts)
}

// Drops anything past the retention window, then anything past the ring bound.
// Age is checked first so a quiet resource's stale history is discarded even
// when the ring never fills.
func trimEvents(events []Event) []Event {
	cutoff := time.Now().UTC().Add(-EventRetention)
	kept := events[:0]
	for _, event := range events {
		if event.Date.After(cutoff) {
			kept = append(kept, event)
		}
	}
	if len(kept) > eventRingSize {
		kept = kept[len(kept)-eventRingSize:]
	}
	return kept
}

// DescribeEvents returns the customer view of the ring. AWS scopes a read to a time window rather than
// to a resource, so an unfiltered call reports the whole account's recent history, including resources
// that have since been deleted.
func (s *Service) DescribeEvents(ctx context.Context, input *rds.DescribeEventsInput, accountID string) (*rds.DescribeEventsOutput, error) {
	// AWS recognises these names. They are not applied, as recognised names are
	// not on the other calls documented as not supporting Filters.
	if _, err := ReadFilters(input.Filters, filterEventCategory, filterDBInstanceID); err != nil {
		return nil, err
	}
	window, err := eventWindow(input)
	if err != nil {
		return nil, err
	}
	sourceType := aws.StringValue(input.SourceType)
	sourceIdentifier := aws.StringValue(input.SourceIdentifier)
	if sourceType != "" && !validEventSourceType(sourceType) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"SourceType %q is not an RDS event source", sourceType)
	}
	// AWS's own rule: an identifier is meaningless without the type, since two
	// resource kinds may share a name.
	if sourceIdentifier != "" && sourceType == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterCombination,
			"SourceType is required when SourceIdentifier is supplied")
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	keys, err := eventRingKeys(ctx, kv, sourceType, sourceIdentifier)
	if err != nil {
		return nil, err
	}

	wanted := aws.StringValueSlice(input.EventCategories)
	var events []Event
	for _, key := range keys {
		var ring eventRing
		found, err := getJSON(ctx, kv, key, &ring)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		for _, event := range ring.Events {
			if event.Date.Before(window.start) || event.Date.After(window.end) {
				continue
			}
			if !matchesCategories(event, wanted) {
				continue
			}
			events = append(events, event)
		}
	}

	// Oldest first across resources, as AWS returns them, so a client that has
	// already read up to a timestamp can resume from it.
	slices.SortFunc(events, func(a, b Event) int {
		return strings.Compare(eventPageKey(a), eventPageKey(b))
	})
	events, marker, err := Page(events, eventPageKey, input.MaxRecords, input.Marker)
	if err != nil {
		return nil, err
	}

	out := &rds.DescribeEventsOutput{Events: make([]*rds.Event, 0, len(events)), Marker: marker}
	for _, event := range events {
		out.Events = append(out.Events, s.projectEvent(accountID, event))
	}
	return out, nil
}

func (s *Service) projectEvent(accountID string, event Event) *rds.Event {
	out := &rds.Event{
		SourceIdentifier: aws.String(event.SourceIdentifier),
		SourceType:       aws.String(event.SourceType),
		Message:          aws.String(event.Message),
		Date:             aws.Time(event.Date),
		EventCategories:  aws.StringSlice(event.Categories),
	}
	switch event.SourceType {
	case EventSourceTypeDBInstance:
		out.SourceArn = aws.String(FormatARN(ResourceKindDBInstance, s.region, accountID, event.SourceIdentifier))
	case EventSourceTypeDBSnapshot:
		out.SourceArn = aws.String(FormatARN(ResourceKindDBSnapshot, s.region, accountID, event.SourceIdentifier))
	}
	return out
}

// The window a read covers. AWS resolves Duration against "now" and lets
// explicit StartTime/EndTime override it.
type timeWindow struct {
	start, end time.Time
}

func eventWindow(input *rds.DescribeEventsInput) (timeWindow, error) {
	now := time.Now().UTC()
	window := timeWindow{start: now.Add(-defaultEventDuration), end: now}

	if minutes := aws.Int64Value(input.Duration); minutes > 0 {
		window.start = now.Add(-time.Duration(minutes) * time.Minute)
	}
	if input.StartTime != nil {
		window.start = input.StartTime.UTC()
	}
	if input.EndTime != nil {
		window.end = input.EndTime.UTC()
	}
	if window.end.Before(window.start) {
		return timeWindow{}, awserrors.Errorf(awserrors.ErrorInvalidParameterCombination, "EndTime is before StartTime")
	}
	return window, nil
}

// Fixed-width nanoseconds lead, so the key sorts by date, and the remaining
// fields break a tie the same way on every read.
func eventPageKey(event Event) string {
	return pageKey(fmt.Sprintf("%020d", event.Date.UnixNano()),
		event.SourceIdentifier, event.SourceType, event.Message)
}

// A fully qualified read is one Get. Anything broader has to enumerate, because
// a ring outlives the record that would otherwise name it.
func eventRingKeys(ctx context.Context, kv *kvstore.Bucket, sourceType, sourceIdentifier string) ([]string, error) {
	if sourceType != "" && sourceIdentifier != "" {
		return []string{EventRingKey(sourceType, sourceIdentifier)}, nil
	}
	prefix := EventsPrefix()
	if sourceType != "" {
		prefix += sourceType + "/"
	}
	keys, err := bucketKeys(ctx, kv)
	if err != nil {
		return nil, fmt.Errorf("rds: list event rings: %w", err)
	}
	matched := make([]string, 0, len(keys))
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			matched = append(matched, key)
		}
	}
	slices.Sort(matched)
	return matched, nil
}

// An empty filter matches everything, as AWS does; otherwise the event needs at
// least one of the requested categories.
func matchesCategories(event Event, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	return slices.ContainsFunc(wanted, func(category string) bool {
		return slices.Contains(event.Categories, category)
	})
}
