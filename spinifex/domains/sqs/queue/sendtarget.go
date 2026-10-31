package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
)

var _ message.Queues = (*Store)(nil)

// SendTarget resolves the active queue a message is sent to, with the delay
// and size limit in effect now. A record whose values cannot be read is
// unavailable rather than defaulted.
func (s *Store) SendTarget(ctx context.Context, account, region, name string) (message.Target, error) {
	r, err := s.Resolve(ctx, account, region, name)
	if errors.Is(err, ErrQueueDoesNotExist) {
		return message.Target{}, fmt.Errorf("%w: %w", message.ErrQueueDoesNotExist, err)
	}
	if err != nil {
		return message.Target{}, fmt.Errorf("%w: %w", message.ErrUnavailable, err)
	}
	delay, err := intAttribute(r, AttrDelaySeconds)
	if err != nil {
		return message.Target{}, err
	}
	size, err := intAttribute(r, AttrMaximumMessageSize)
	if err != nil {
		return message.Target{}, err
	}
	return message.Target{Namespace: r.Namespace(), Account: r.Account, DelaySeconds: delay, MaximumMessageSize: size}, nil
}

func intAttribute(r Record, name string) (int, error) {
	v, ok := r.Attributes[name]
	if !ok {
		return 0, fmt.Errorf("%w: queue %s has no %s", message.ErrUnavailable, r.QueueID, name)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%w: queue %s %s is not an integer", message.ErrUnavailable, r.QueueID, name)
	}
	return n, nil
}

var _ message.ReceiveQueues = (*Store)(nil)

// ReceiveTarget resolves the active queue a receive reads, with the visibility
// timeout, retention and receive wait in effect now. A record whose values
// cannot be read is unavailable rather than defaulted.
func (s *Store) ReceiveTarget(ctx context.Context, account, region, name string) (message.ReceiveTarget, error) {
	r, err := s.Resolve(ctx, account, region, name)
	if errors.Is(err, ErrQueueDoesNotExist) {
		return message.ReceiveTarget{}, fmt.Errorf("%w: %w", message.ErrQueueDoesNotExist, err)
	}
	if err != nil {
		return message.ReceiveTarget{}, fmt.Errorf("%w: %w", message.ErrUnavailable, err)
	}
	vis, err := intAttribute(r, AttrVisibilityTimeout)
	if err != nil {
		return message.ReceiveTarget{}, err
	}
	retention, err := intAttribute(r, AttrMessageRetentionPeriod)
	if err != nil {
		return message.ReceiveTarget{}, err
	}
	wait, err := intAttribute(r, AttrReceiveMessageWaitTimeSeconds)
	if err != nil {
		return message.ReceiveTarget{}, err
	}
	return message.ReceiveTarget{
		Namespace: r.Namespace(), Account: r.Account,
		VisibilityTimeout: vis, MessageRetentionPeriod: retention, ReceiveWaitTime: wait,
	}, nil
}
