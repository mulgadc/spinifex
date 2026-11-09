package kvstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// msgGetResponse is the JetStream API reply to a stream message get.
type msgGetResponse struct {
	Error   *jetstream.APIError `json:"error,omitempty"`
	Message *struct {
		Subject  string    `json:"subject"`
		Sequence uint64    `json:"seq"`
		Header   []byte    `json:"hdrs,omitempty"`
		Data     []byte    `json:"data,omitempty"`
		Time     time.Time `json:"time"`
	} `json:"message,omitempty"`
}

// MsgGetRequest selects one message for a stream message get.
type MsgGetRequest struct {
	Seq     uint64 `json:"seq,omitempty"`
	LastFor string `json:"last_by_subj,omitempty"`
	NextFor string `json:"next_by_subj,omitempty"`
}

// LeaderMsgGet sends one stream message get, which only the stream leader
// answers; nats.go switches to a direct get, which followers also answer,
// whenever the stream allows it. A filter matching nothing is jetstream.ErrMsgNotFound.
func LeaderMsgGet(ctx context.Context, js jetstream.JetStream, stream string, sel MsgGetRequest) (*jetstream.RawStreamMsg, error) {
	opts := js.Options()
	if _, ok := ctx.Deadline(); !ok && opts.DefaultTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.DefaultTimeout)
		defer cancel()
	}
	req, err := json.Marshal(sel)
	if err != nil {
		return nil, err
	}
	reply, err := js.Conn().RequestWithContext(ctx, APIPrefix(opts)+"STREAM.MSG.GET."+stream, req)
	if err != nil {
		return nil, err
	}
	var resp msgGetResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return nil, fmt.Errorf("decode message get reply: %w", err)
	}
	if resp.Error != nil {
		// Matches jetstream.ErrMsgNotFound by error code when the subject is empty.
		return nil, resp.Error
	}
	if resp.Message == nil {
		return nil, jetstream.ErrInvalidJetStreamResponse
	}
	var hdr nats.Header
	if len(resp.Message.Header) > 0 {
		if hdr, err = nats.DecodeHeadersMsg(resp.Message.Header); err != nil {
			return nil, fmt.Errorf("decode message get headers: %w", err)
		}
	}
	return &jetstream.RawStreamMsg{
		Subject: resp.Message.Subject, Sequence: resp.Message.Sequence,
		Header: hdr, Data: resp.Message.Data, Time: resp.Message.Time,
	}, nil
}

// APIPrefix is the request prefix nats.go itself uses for js.
func APIPrefix(o jetstream.JetStreamOptions) string {
	switch {
	case o.Domain != "":
		return "$JS." + o.Domain + ".API."
	case o.APIPrefix != "":
		return strings.TrimSuffix(o.APIPrefix, ".") + "."
	}
	return jetstream.DefaultAPIPrefix
}
