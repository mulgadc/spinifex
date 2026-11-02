package message_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var claimNow = time.UnixMilli(1_800_000_000_000)

// stateStore holds one message and its state record in memory, with
// injectable read and write failures; other Store methods are unused.
type stateStore struct {
	message.Store

	msg      *message.Message
	state    []byte
	rev      message.Revision
	getErr   error
	stateErr error
	putErr   error
	puts     int
	// recheck, when set, answers every body read after the first.
	recheck   error
	deleteErr error
	gets      int
	deletes   int
}

func (s *stateStore) Get(_ context.Context, _ message.Namespace, pos message.Position) (message.Message, error) {
	s.gets++
	if s.gets > 1 && s.recheck != nil {
		return message.Message{}, s.recheck
	}
	if s.getErr != nil {
		return message.Message{}, s.getErr
	}
	if s.msg == nil || s.msg.Position != pos {
		return message.Message{}, message.ErrNotFound
	}
	return *s.msg, nil
}

func (s *stateStore) GetState(context.Context, message.Namespace, message.Position) (message.State, error) {
	if s.stateErr != nil {
		return message.State{}, s.stateErr
	}
	if s.rev == 0 {
		return message.State{}, message.ErrNotFound
	}
	return message.State{Value: s.state, Revision: s.rev}, nil
}

func (s *stateStore) PutState(_ context.Context, _ message.Namespace, _ message.Position, v []byte, expected message.Revision) (message.Revision, error) {
	s.puts++
	if s.putErr != nil {
		return 0, s.putErr
	}
	if expected != s.rev {
		return 0, message.ErrRevisionMismatch
	}
	s.state, s.rev = v, s.rev+1
	return s.rev, nil
}

func (s *stateStore) DeleteState(_ context.Context, _ message.Namespace, _ message.Position, expected message.Revision) error {
	s.deletes++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if expected != s.rev {
		return message.ErrRevisionMismatch
	}
	s.state, s.rev = nil, 0
	return nil
}

func ms(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func storedMessage(enq, vis time.Time) *message.Message {
	return &message.Message{Position: 7, Body: []byte("hello"), Meta: map[string]string{
		message.MetaV: "1", message.MetaID: "id-1", message.MetaEnq: ms(enq), message.MetaVis: ms(vis),
		message.MetaSender: "AIDAEXAMPLE", message.MetaMD5: "5d41402abc4b2a76b9719d911017c592",
	}}
}

func claimer(s *stateStore, now time.Time) *message.Claimer {
	return &message.Claimer{Store: s, Now: func() time.Time { return now }}
}

func claimIn(vt time.Duration) message.ClaimInput {
	return message.ClaimInput{Namespace: sendNS, Position: 7, Visibility: vt, Retention: 4 * 24 * time.Hour}
}

func decoded(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func TestClaimFirstReceive(t *testing.T) {
	s := &stateStore{msg: storedMessage(claimNow.Add(-time.Minute), claimNow.Add(-time.Minute))}
	got, err := claimer(s, claimNow).Claim(context.Background(), claimIn(30*time.Second))
	require.NoError(t, err)
	assert.Equal(t, "id-1", got.MessageID)
	assert.Equal(t, []byte("hello"), got.Message.Body)
	assert.Equal(t, uint64(1), got.ReceiveCount)
	assert.Equal(t, uint64(1), got.Generation)
	assert.Equal(t, claimNow.Add(30*time.Second).UnixMilli(), got.Deadline.UnixMilli())
	assert.Equal(t, claimNow.UnixMilli(), got.ReceivedAt.UnixMilli())
	assert.Equal(t, claimNow.UnixMilli(), got.FirstReceive.UnixMilli())
	assert.Equal(t, message.Revision(1), got.Revision)
	n := float64(claimNow.UnixMilli())
	assert.Equal(t, map[string]any{"v": 1.0, "s": "inflight", "c": 1.0, "g": 1.0, "d": n + 30_000, "r": n, "f": n}, decoded(t, s.state))
}

func TestClaimDelayAndRetention(t *testing.T) {
	s := &stateStore{msg: storedMessage(claimNow.Add(-time.Minute), claimNow.Add(time.Millisecond))}
	_, err := claimer(s, claimNow).Claim(context.Background(), claimIn(time.Second))
	require.ErrorIs(t, err, message.ErrNotVisible)
	_, err = claimer(s, claimNow.Add(time.Millisecond)).Claim(context.Background(), claimIn(time.Second))
	require.NoError(t, err, "visible exactly at delay-until")

	old := &stateStore{msg: storedMessage(claimNow.Add(-4*24*time.Hour), claimNow.Add(-4*24*time.Hour))}
	_, err = claimer(old, claimNow).Claim(context.Background(), claimIn(time.Second))
	require.ErrorIs(t, err, message.ErrGone, "expired exactly at enqueue plus retention")
	assert.Zero(t, old.puts)
	_, err = claimer(old, claimNow.Add(-time.Millisecond)).Claim(context.Background(), claimIn(time.Second))
	require.NoError(t, err)
}

func inflight(c, g uint64, d, r, f time.Time) []byte {
	b, _ := json.Marshal(map[string]any{"v": 1, "s": "inflight", "c": c, "g": g, "d": d.UnixMilli(), "r": r.UnixMilli(), "f": f.UnixMilli()})
	return b
}

func TestClaimInflightAndExpiry(t *testing.T) {
	first := claimNow.Add(-time.Hour)
	s := &stateStore{msg: storedMessage(first, first), state: inflight(2, 5, claimNow.Add(time.Millisecond), first, first), rev: 9}
	_, err := claimer(s, claimNow).Claim(context.Background(), claimIn(time.Second))
	require.ErrorIs(t, err, message.ErrNotVisible)
	assert.Zero(t, s.puts)

	got, err := claimer(s, claimNow.Add(time.Millisecond)).Claim(context.Background(), claimIn(time.Second))
	require.NoError(t, err, "visible again exactly at the deadline")
	assert.Equal(t, uint64(3), got.ReceiveCount)
	assert.Equal(t, uint64(6), got.Generation)
	assert.Equal(t, first.UnixMilli(), got.FirstReceive.UnixMilli(), "first receive time is kept")
	assert.Equal(t, claimNow.Add(time.Millisecond).UnixMilli(), got.ReceivedAt.UnixMilli())
	assert.Equal(t, message.Revision(10), got.Revision)
}

func TestClaimRefusesWhatIsNotOurs(t *testing.T) {
	for name, tc := range map[string]struct {
		state   string
		meta    map[string]string
		wantErr error
	}{
		"unknown state":           {state: `{"v":1,"s":"archived"}`, wantErr: message.ErrNotOurs},
		"newer state version":     {state: `{"v":2,"s":"inflight","c":1,"g":1,"d":1,"r":1,"f":1}`, wantErr: message.ErrNotOurs},
		"newer body version":      {meta: map[string]string{message.MetaV: strconv.Itoa(message.MetaVersion + 1)}, wantErr: message.ErrNotOurs},
		"attributes without MD5":  {meta: map[string]string{message.MetaV: "2"}, wantErr: message.ErrUnavailable},
		"plain with attrs MD5":    {meta: map[string]string{message.MetaMD5A: strings.Repeat("0", 32)}, wantErr: message.ErrUnavailable},
		"not an object":           {state: `[]`, wantErr: message.ErrUnavailable},
		"null":                    {state: `null`, wantErr: message.ErrUnavailable},
		"no version":              {state: `{"s":"deleted"}`, wantErr: message.ErrUnavailable},
		"zero version":            {state: `{"v":0,"s":"deleted"}`, wantErr: message.ErrUnavailable},
		"no state":                {state: `{"v":1}`, wantErr: message.ErrUnavailable},
		"wrong field type":        {state: `{"v":1,"s":"inflight","c":"1"}`, wantErr: message.ErrUnavailable},
		"inflight without count":  {state: `{"v":1,"s":"inflight","g":1,"d":1,"r":1,"f":1}`, wantErr: message.ErrUnavailable},
		"inflight without gen":    {state: `{"v":1,"s":"inflight","c":1,"d":1,"r":1,"f":1}`, wantErr: message.ErrUnavailable},
		"inflight without dead":   {state: `{"v":1,"s":"inflight","c":1,"g":1,"r":1,"f":1}`, wantErr: message.ErrUnavailable},
		"inflight without recv":   {state: `{"v":1,"s":"inflight","c":1,"g":1,"d":1,"f":1}`, wantErr: message.ErrUnavailable},
		"inflight without first":  {state: `{"v":1,"s":"inflight","c":1,"g":1,"d":1,"r":1}`, wantErr: message.ErrUnavailable},
		"first after receive":     {state: `{"v":1,"s":"inflight","c":1,"g":1,"d":1,"r":1,"f":2}`, wantErr: message.ErrUnavailable},
		"exhausted generation":    {state: `{"v":1,"s":"inflight","c":1,"g":18446744073709551615,"d":1,"r":1,"f":1}`, wantErr: message.ErrUnavailable},
		"body without version":    {meta: map[string]string{message.MetaV: ""}, wantErr: message.ErrUnavailable},
		"body without id":         {meta: map[string]string{message.MetaID: ""}, wantErr: message.ErrUnavailable},
		"body without enqueue":    {meta: map[string]string{message.MetaEnq: "x"}, wantErr: message.ErrUnavailable},
		"delay before enqueue":    {meta: map[string]string{message.MetaVis: "1"}, wantErr: message.ErrUnavailable},
		"malformed carried count": {meta: map[string]string{message.MetaRc: "-1"}, wantErr: message.ErrUnavailable},
		"body without sender":     {meta: map[string]string{message.MetaSender: ""}, wantErr: message.ErrUnavailable},
		"body without md5":        {meta: map[string]string{message.MetaMD5: ""}, wantErr: message.ErrUnavailable},
		"body with short md5":     {meta: map[string]string{message.MetaMD5: "5d41402abc4b2a76"}, wantErr: message.ErrUnavailable},
		"body with upper md5":     {meta: map[string]string{message.MetaMD5: "5D41402ABC4B2A76B9719D911017C592"}, wantErr: message.ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			s := &stateStore{msg: storedMessage(claimNow.Add(-time.Minute), claimNow.Add(-time.Minute))}
			maps.Copy(s.msg.Meta, tc.meta)
			if tc.state != "" {
				s.state, s.rev = []byte(tc.state), 3
			}
			_, err := claimer(s, claimNow).Claim(context.Background(), claimIn(time.Second))
			require.ErrorIs(t, err, tc.wantErr)
			assert.Zero(t, s.puts, "nothing is written")
		})
	}
}

func TestClaimPreservesUnknownFields(t *testing.T) {
	first := claimNow.Add(-time.Hour)
	f := ms(first)
	b := []byte(`{"v":1,"s":"inflight","c":1,"g":1,"d":` + f + `,"r":` + f + `,"f":` + f + `,"grp":{"future":[1,"x"]},"zz":"<kept>"}`)
	s := &stateStore{msg: storedMessage(first, first), state: b, rev: 2}

	_, err := claimer(s, claimNow).Claim(context.Background(), claimIn(time.Second))
	require.NoError(t, err)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(s.state, &got))
	assert.JSONEq(t, `{"future":[1,"x"]}`, string(got["grp"]))
	assert.Equal(t, `"<kept>"`, string(got["zz"]), "unknown fields are kept byte for byte")
	assert.Equal(t, `2`, string(got["g"]))
}

func TestClaimWriteOutcomes(t *testing.T) {
	enq := claimNow.Add(-time.Minute)
	lost := &stateStore{msg: storedMessage(enq, enq), putErr: message.ErrRevisionMismatch}
	_, err := claimer(lost, claimNow).Claim(context.Background(), claimIn(time.Second))
	require.ErrorIs(t, err, message.ErrClaimLost)

	unknown := &stateStore{msg: storedMessage(enq, enq), putErr: errors.New("nats: timeout")}
	_, err = claimer(unknown, claimNow).Claim(context.Background(), claimIn(time.Second))
	require.ErrorIs(t, err, message.ErrUnavailable)
	assert.Equal(t, 1, unknown.puts, "an unknown outcome is never resent")
}
