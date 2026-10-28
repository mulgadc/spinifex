package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
)

// msgGetResponse is the JetStream API reply to a stream message get.
type msgGetResponse struct {
	Error   *jsapi.APIError `json:"error,omitempty"`
	Message *struct {
		Subject  string    `json:"subject"`
		Sequence uint64    `json:"seq"`
		Header   []byte    `json:"hdrs,omitempty"`
		Data     []byte    `json:"data,omitempty"`
		Time     time.Time `json:"time"`
	} `json:"message,omitempty"`
}

// leaderLastMsg returns the last message on subject through a stream
// message get, which only the stream leader answers. nats.go switches to a
// direct get, which followers also answer, whenever the stream allows it.
func leaderLastMsg(ctx context.Context, js jsapi.JetStream, stream, subject string) (*jsapi.RawStreamMsg, error) {
	opts := js.Options()
	if _, ok := ctx.Deadline(); !ok && opts.DefaultTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.DefaultTimeout)
		defer cancel()
	}
	req, err := json.Marshal(struct {
		LastFor string `json:"last_by_subj"`
	}{subject})
	if err != nil {
		return nil, err
	}
	reply, err := js.Conn().RequestWithContext(ctx, apiPrefix(opts)+"STREAM.MSG.GET."+stream, req)
	if err != nil {
		return nil, err
	}
	var resp msgGetResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return nil, fmt.Errorf("decode message get reply: %w", err)
	}
	if resp.Error != nil {
		// Matches jsapi.ErrMsgNotFound by error code when the subject is empty.
		return nil, resp.Error
	}
	if resp.Message == nil {
		return nil, jsapi.ErrInvalidJetStreamResponse
	}
	var hdr nats.Header
	if len(resp.Message.Header) > 0 {
		if hdr, err = nats.DecodeHeadersMsg(resp.Message.Header); err != nil {
			return nil, fmt.Errorf("decode message get headers: %w", err)
		}
	}
	return &jsapi.RawStreamMsg{
		Subject: resp.Message.Subject, Sequence: resp.Message.Sequence,
		Header: hdr, Data: resp.Message.Data, Time: resp.Message.Time,
	}, nil
}

// apiPrefix is the request prefix nats.go itself uses for js.
func apiPrefix(o jsapi.JetStreamOptions) string {
	switch {
	case o.Domain != "":
		return "$JS." + o.Domain + ".API."
	case o.APIPrefix != "":
		return strings.TrimSuffix(o.APIPrefix, ".") + "."
	}
	return jsapi.DefaultAPIPrefix
}

// kvRemoved reports a key's delete or purge marker, as nats.go's KV Get
// reads one.
func kvRemoved(h nats.Header) bool {
	if op := h.Get("KV-Operation"); op != "" {
		return op == "DEL" || op == "PURGE"
	}
	switch h.Get(jsapi.MarkerReasonHeader) {
	case "MaxAge", "Purge", "Remove":
		return true
	}
	return false
}
