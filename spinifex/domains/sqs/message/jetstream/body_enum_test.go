package jetstream_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// messageStreamRequests splits recorded API subjects for one message stream
// into leader message gets, stream infos and anything else.
func messageStreamRequests(subjects []string, stream string) (leaderGets, infos int, other []string) {
	for _, subj := range subjects {
		switch {
		case !strings.HasSuffix(subj, "."+stream) && !strings.Contains(subj, "."+stream+"."):
		case subj == "$JS.API.STREAM.MSG.GET."+stream:
			leaderGets++
		case subj == "$JS.API.STREAM.INFO."+stream:
			infos++
		default:
			other = append(other, subj)
		}
	}
	return leaderGets, infos, other
}

// TestBodyEnumerationIsLeaderServed pins the route Namespaces and Scan use
// for bodies: one leader message get per stored body through the bound, with
// no direct get and no consumer on the message stream.
func TestBodyEnumerationIsLeaderServed(t *testing.T) {
	srv, js := startServer(t, t.TempDir())
	s, err := sqsjs.New(context.Background(), js, testConfig)
	require.NoError(t, err)
	ctx := context.Background()
	a, b := ns(0, 0, "body-a"), ns(0, 1, "body-b")
	var aPos []message.Position
	for range 3 {
		aPos = append(aPos, appendTo(t, s, a, "a"))
		appendTo(t, s, b, "b")
	}
	// Two removed positions leave gaps the enumeration must step over.
	require.NoError(t, s.Delete(ctx, a, aPos[0]))
	require.NoError(t, s.Delete(ctx, a, aPos[2]))
	stream := sqsjs.StreamName(0)

	rec := recordAPI(t, srv.ClientURL())
	rec.drain(t, js.Conn())
	got, err := s.Namespaces(ctx, 0)
	require.NoError(t, err)
	usage := map[message.QueueID]uint64{}
	for _, n := range got {
		usage[n.Namespace.Queue] = n.Usage.Messages
	}
	assert.Equal(t, map[message.QueueID]uint64{a.Queue: 1, b.Queue: 3}, usage)
	gets, infos, rest := messageStreamRequests(rec.drain(t, js.Conn()), stream)
	// Four stored bodies; the last is the bound, so no further get follows.
	assert.Equal(t, 4, gets)
	assert.Equal(t, 1, infos)
	assert.Empty(t, rest, "Namespaces sent other requests for the message stream")

	page, err := s.Scan(ctx, 0, message.ScanCursor{}, 3)
	require.NoError(t, err)
	require.Len(t, page.Changes, 3)
	assert.False(t, page.Done)
	gets, infos, rest = messageStreamRequests(rec.drain(t, js.Conn()), stream)
	assert.Equal(t, 3, gets, "a full page reads one body per change")
	assert.Equal(t, 1, infos, "the first page captures the bound")
	assert.Empty(t, rest, "Scan sent other requests for the message stream")

	page, err = s.Scan(ctx, 0, page.Next, 3)
	require.NoError(t, err)
	require.Len(t, page.Changes, 1)
	assert.True(t, page.Done)
	gets, infos, rest = messageStreamRequests(rec.drain(t, js.Conn()), stream)
	assert.Equal(t, 1, gets)
	assert.Zero(t, infos, "later pages keep the first page's bound")
	assert.Empty(t, rest)
}

// TestScanStepsOverGapsInOrderWithinTheBound pages through removed positions
// and neighbouring namespaces in stream order, and stops at the first page's
// bound even when later bodies exist.
func TestScanStepsOverGapsInOrderWithinTheBound(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	a, b := ns(0, 0, "gap-a"), ns(0, 0, "gap-b")
	var want []message.Position
	for i := range 6 {
		pa := appendTo(t, s, a, "a")
		pb := appendTo(t, s, b, "b")
		if i%2 == 0 {
			require.NoError(t, s.Delete(ctx, a, pa))
		} else {
			want = append(want, pa)
		}
		want = append(want, pb)
	}
	page, err := s.Scan(ctx, 0, message.ScanCursor{}, 4)
	require.NoError(t, err)
	// Appended after the bound is fixed, so never part of this pass.
	appendTo(t, s, a, "late")
	var got []message.Position
	for {
		for _, c := range page.Changes {
			got = append(got, c.Position)
			require.Equal(t, message.Cursor(c.Position), c.Cursor)
		}
		if page.Done {
			break
		}
		page, err = s.Scan(ctx, 0, page.Next, 4)
		require.NoError(t, err)
	}
	assert.Equal(t, want, got)
}

func TestBodyEnumerationCancelled(t *testing.T) {
	s, _ := newStore(t)
	appendTo(t, s, ns(0, 0, "cancel"), "b")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Namespaces(ctx, 0)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.Scan(ctx, 0, message.ScanCursor{}, 10)
	require.ErrorIs(t, err, context.Canceled)
}
