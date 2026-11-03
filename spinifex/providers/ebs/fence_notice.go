package ebsprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// NotifyFenced sends notice once and returns the instance the daemon stopped.
// An ErrInvalidArgument error is terminal: the notice can never be sent. A
// *ProviderError (internal for an undecodable ack) was heard; others were not.
func NotifyFenced(ctx context.Context, conn *nats.Conn, notice FenceNotice, timeout time.Duration) (string, error) {
	if notice.VolumeID == "" {
		return "", fmt.Errorf("%w: fence notice names no volume", ErrInvalidArgument)
	}
	subject, err := FenceNoticeSubject(notice.NodeID)
	if err != nil {
		return "", err
	}
	if conn == nil {
		return "", fmt.Errorf("%w: no connection to send the fence notice on", ErrInvalidArgument)
	}

	reply, err := requestReply(ctx, conn, subject, timeout, notice)
	if err != nil {
		return "", err
	}
	var ack FenceNoticeAck
	if err := json.Unmarshal(reply, &ack); err != nil {
		// The daemon answered, so repeating the notice would only repeat this.
		return "", &ProviderError{Code: ErrorCodeInternal, Message: fmt.Sprintf("decode %s ack: %v", subject, err)}
	}
	if ack.Error != nil {
		return ack.InstanceID, ack.Error
	}
	return ack.InstanceID, nil
}

// FenceNoticeRetryable reports whether a NotifyFenced error leaves the notice
// to be sent again: it was not heard, or the daemon refused it as unavailable.
func FenceNoticeRetryable(err error) bool {
	if err == nil || errors.Is(err, ErrInvalidArgument) {
		return false
	}
	if _, refused := errors.AsType[*ProviderError](err); refused {
		return errors.Is(err, ErrUnavailable)
	}
	return true
}
