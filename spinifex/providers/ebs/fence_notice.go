package ebsprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// NotifyFenced sends notice once to the daemon on notice.NodeID and returns the
// instance it stopped, or "" if none. A *ProviderError means the daemon heard
// the notice and refused it; any other error means it was not heard.
func NotifyFenced(ctx context.Context, conn *nats.Conn, notice FenceNotice, timeout time.Duration) (string, error) {
	if notice.VolumeID == "" {
		return "", fmt.Errorf("%w: fence notice names no volume", ErrInvalidArgument)
	}
	subject, err := FenceNoticeSubject(notice.NodeID)
	if err != nil {
		return "", err
	}
	if conn == nil {
		return "", nats.ErrConnectionClosed
	}
	payload, err := json.Marshal(notice)
	if err != nil {
		return "", fmt.Errorf("encode %s notice: %w", subject, err)
	}

	request := &nats.Msg{Subject: subject, Data: payload, Header: nats.Header{}}
	ctx, span := startClientSpan(ctx, subject, request.Header)
	defer span.End()

	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msg, err := conn.RequestMsgWithContext(requestCtx, request)
	if err != nil {
		RecordSpanError(span, err)
		return "", fmt.Errorf("request %s: %w", subject, err)
	}
	RecordResponseError(span, msg.Data)

	var ack FenceNoticeAck
	if err := json.Unmarshal(msg.Data, &ack); err != nil {
		// The daemon answered, so repeating the notice would only repeat this.
		return "", &ProviderError{Code: ErrorCodeInternal, Message: fmt.Sprintf("decode %s ack: %v", subject, err)}
	}
	if ack.Error != nil {
		return ack.InstanceID, ack.Error
	}
	return ack.InstanceID, nil
}
