package message_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sendAcct = "000000000001"

var sendNS = message.Namespace{MessageShard: 0, StateShard: 0, Queue: "q1"}

// recordingStore answers Append with a fixed result and counts the calls;
// every other Store method is unused by Send.
type recordingStore struct {
	message.Store

	appends int
	res     message.AppendResult
	err     error
}

func (r *recordingStore) Append(context.Context, message.Namespace, message.AppendInput) (message.AppendResult, error) {
	r.appends++
	return r.res, r.err
}

type fixedTarget struct {
	t   message.Target
	err error
}

func (f fixedTarget) SendTarget(context.Context, string, string, string) (message.Target, error) {
	return f.t, f.err
}

func goodTarget() message.Target {
	return message.Target{Namespace: sendNS, Account: sendAcct, MaximumMessageSize: 1024}
}

func sendInput(body string) message.SendInput {
	return message.SendInput{Account: sendAcct, Region: "us-east-1", Queue: "orders", Sender: "AIDAEXAMPLE", Body: []byte(body)}
}

func send(store *recordingStore, tgt message.Target, in message.SendInput) error {
	s := &message.Sender{Store: store, Queues: fixedTarget{t: tgt}}
	_, err := s.Send(context.Background(), in)
	return err
}

func TestSendRejectsUnimplementedFeatures(t *testing.T) {
	one := "1"
	attr := map[string]message.AttributeValue{"a": {DataType: "String", StringValue: &one}}
	for name, mutate := range map[string]func(*message.SendInput){
		"Number message attributes": func(in *message.SendInput) {
			in.MessageAttributes = []message.Attribute{{Name: "a", Value: message.AttributeValue{DataType: "Number.int", StringValue: &one}}}
		},
		"message system attributes": func(in *message.SendInput) { in.MessageSystemAttributes = attr },
		"MessageGroupId":            func(in *message.SendInput) { g := ""; in.MessageGroupID = &g },
		"MessageDeduplicationId":    func(in *message.SendInput) { d := ""; in.MessageDeduplicationID = &d },
	} {
		t.Run(name, func(t *testing.T) {
			st := &recordingStore{res: message.AppendResult{Position: 1}}
			in := sendInput("x")
			mutate(&in)
			require.ErrorIs(t, send(st, goodTarget(), in), message.ErrUnsupported)
			assert.Zero(t, st.appends)
		})
	}
}

func TestSendRejectsDisallowedContents(t *testing.T) {
	for name, body := range map[string][]byte{
		"NUL":           {'a', 0x00},
		"control 0x08":  {0x08},
		"U+FFFE":        []byte("￾"),
		"U+FFFF":        []byte("￿"),
		"invalid UTF-8": {0xff, 'a'},
		"surrogate":     {0xed, 0xa0, 0x80},
	} {
		t.Run(name, func(t *testing.T) {
			st := &recordingStore{res: message.AppendResult{Position: 1}}
			in := sendInput("")
			in.Body = body
			require.ErrorIs(t, send(st, goodTarget(), in), message.ErrInvalidContents)
			assert.Zero(t, st.appends)
		})
	}
}

func TestSendAppendResultsThatAreNotSuccess(t *testing.T) {
	for name, st := range map[string]*recordingStore{
		"refused append": {err: errors.New("refused")},
		"no position":    {},
		"duplicate":      {res: message.AppendResult{Position: 7, Duplicate: true}},
	} {
		t.Run(name, func(t *testing.T) {
			s := &message.Sender{Store: st, Queues: fixedTarget{t: goodTarget()}}
			res, err := s.Send(context.Background(), sendInput("x"))
			require.ErrorIs(t, err, message.ErrUnavailable)
			assert.Zero(t, res)
			assert.Equal(t, 1, st.appends, "exactly one append, never a resend")
		})
	}
}
