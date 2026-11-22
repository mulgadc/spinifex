//go:build e2e

package storagefault

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

type fakeLeaseEntry struct {
	jetstream.KeyValueEntry
	value []byte
	rev   uint64
}

func (e fakeLeaseEntry) Value() []byte    { return e.value }
func (e fakeLeaseEntry) Revision() uint64 { return e.rev }

type fakeLeaseKV struct {
	jetstream.KeyValue
	entry jetstream.KeyValueEntry
	err   error
}

func (kv fakeLeaseKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return kv.entry, kv.err
}

func leaseAt(owner string, gen, rev uint64) fakeLeaseKV {
	v := `{"owner":"` + owner + `","generation":` + strconv.FormatUint(gen, 10) + `}`
	return fakeLeaseKV{entry: fakeLeaseEntry{value: []byte(v), rev: rev}}
}

func TestGuardCheck(t *testing.T) {
	atKill := leaseSnapshot{Owner: "node2", Generation: 123}
	fresh := func() time.Time { return time.Now() }
	stale := func() time.Time { return time.Now().Add(-leaseGuardValidity - time.Second) }
	cases := []struct {
		name      string
		kv        fakeLeaseKV
		renewedAt time.Time
		wantStop  string
	}{
		{"renewed by the same holder", leaseAt("node2", 123, 50), stale(), ""},
		{"same revision, recently renewed", leaseAt("node2", 123, 40), fresh(), ""},
		{"same revision for too long", leaseAt("node2", 123, 40), stale(), "no renewal seen"},
		{"another owner", leaseAt("node3", 124, 60), fresh(), "lease is now node3"},
		{"a new generation on this node", leaseAt("node2", 124, 60), fresh(), "generation 124"},
		{"entry deleted", fakeLeaseKV{err: jetstream.ErrKeyNotFound}, fresh(), "lease entry is gone"},
		{"read stalls, recently renewed", fakeLeaseKV{err: context.DeadlineExceeded}, fresh(), ""},
		{"read stalls past validity", fakeLeaseKV{err: errors.New("timeout")}, stale(), "no renewal seen"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &replacement{}
			lastRev, renewedAt := uint64(40), c.renewedAt
			got := r.check(context.Background(), c.kv, "vol-1", atKill, &lastRev, &renewedAt)
			if c.wantStop == "" && got != "" {
				t.Fatalf("check stopped the replacement: %s", got)
			}
			if c.wantStop != "" && !strings.Contains(got, c.wantStop) {
				t.Fatalf("check = %q, want it to contain %q", got, c.wantStop)
			}
		})
	}
}
