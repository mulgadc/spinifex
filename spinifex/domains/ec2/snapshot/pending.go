package snapshot

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

// The snapshot document states CreateSnapshot and the reaper move between.
const (
	snapshotStatePending   = string(ebsprovider.SnapshotStatePending)
	snapshotStateCompleted = string(ebsprovider.SnapshotStateCompleted)
	snapshotStateError     = string(ebsprovider.SnapshotStateError)
)

// definiteRefusal reports a CreateSnapshot error that is the provider's answer
// that it holds no snapshot for the call. Anything else, a timeout, a lost
// connection or unavailable, may follow an accepted call.
func definiteRefusal(err error) bool {
	return errors.Is(err, ebsprovider.ErrInvalidArgument) || errors.Is(err, ebsprovider.ErrNotFound) ||
		errors.Is(err, ebsprovider.ErrAlreadyExists) || errors.Is(err, ebsprovider.ErrVolumeInUse)
}

// refusalSettlesError reports a refusal no later call can change: the source
// volume is gone, or the ID is another volume's snapshot. Only these make a
// pending document error once an earlier call may still be accepted.
func refusalSettlesError(err error) bool {
	return errors.Is(err, ebsprovider.ErrNotFound) || errors.Is(err, ebsprovider.ErrAlreadyExists)
}

// applyProviderAnswer moves a document to what the provider answered: completed
// at 100%, or pending with the provider's handle for the reaper to follow. The
// call was answered, so AmbiguousCreate stays only if an earlier one was not.
func applyProviderAnswer(created *ebsprovider.Snapshot, earlierUnanswered bool) func(*ebsmetadata.Snapshot) {
	return func(doc *ebsmetadata.Snapshot) {
		doc.AmbiguousCreate = earlierUnanswered
		if created.Handle != "" {
			doc.ProviderHandle = created.Handle
		}
		if created.State == ebsprovider.SnapshotStateCompleted {
			doc.State = snapshotStateCompleted
			doc.Progress = "100%"
		}
	}
}

// repeatOutcome is the write a CreateSnapshot answer after an unanswered call
// makes: answered, the provider's answer; a refusal no later call can change,
// error; anything else, nil, leaving the document pending.
func repeatOutcome(created *ebsprovider.Snapshot, err error, earlierUnanswered bool) (mutate func(*ebsmetadata.Snapshot), answered bool) {
	switch {
	case err == nil && created != nil:
		return applyProviderAnswer(created, earlierUnanswered), true
	case refusalSettlesError(err):
		return func(d *ebsmetadata.Snapshot) {
			markSnapshotError(d)
			d.AmbiguousCreate = earlierUnanswered
		}, false
	}
	return nil, false
}

func markSnapshotCompleted(doc *ebsmetadata.Snapshot) {
	doc.State = snapshotStateCompleted
	doc.Progress = "100%"
}

func markSnapshotError(doc *ebsmetadata.Snapshot) {
	doc.State = snapshotStateError
}

// updatePending rereads a snapshot document and applies mutate only while it
// is still pending, so a delete or settlement since the caller read it is not
// undone. It returns the document as read or written, zero when it is gone.
func (s *SnapshotServiceImpl) updatePending(ctx context.Context, accountID, snapshotID string, mutate func(*ebsmetadata.Snapshot)) (ebsmetadata.Snapshot, bool, error) {
	return s.rewrite(ctx, accountID, snapshotID, func(doc *ebsmetadata.Snapshot) bool {
		// A marked document is the reaper's to delete, never to settle.
		if doc.State != snapshotStatePending || doc.Hidden() {
			return false
		}
		mutate(doc)
		return true
	})
}

// rewrite rereads snapshotID's document and writes it back when mutate asks
// to. It returns the document as read or written and whether it wrote; one
// that is gone is zero, and mutate never sees it.
func (s *SnapshotServiceImpl) rewrite(ctx context.Context, accountID, snapshotID string, mutate func(*ebsmetadata.Snapshot) bool) (ebsmetadata.Snapshot, bool, error) {
	doc, err := s.metadata.GetSnapshot(ctx, accountID, snapshotID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return ebsmetadata.Snapshot{}, false, nil
		}
		return ebsmetadata.Snapshot{}, false, fmt.Errorf("read snapshot %s: %w", snapshotID, err)
	}
	if !mutate(&doc) {
		return doc, false, nil
	}
	if err := s.metadata.PutSnapshot(ctx, doc); err != nil {
		return ebsmetadata.Snapshot{}, false, fmt.Errorf("write snapshot %s: %w", snapshotID, err)
	}
	return doc, true, nil
}

// writeContext bounds a write made after the provider answered, which has to
// land even when the request that asked has been cancelled.
func writeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
}
