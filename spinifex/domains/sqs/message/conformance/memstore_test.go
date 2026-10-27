package conformance_test

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
)

// memData is the durable state a reopened memStore sees again, standing in
// for what a real store would hold on disk across a crash.
type memData struct {
	mu       sync.Mutex
	limits   message.Limits
	clock    time.Duration
	seq      map[message.Shard]uint64
	messages map[message.Namespace]map[message.Position]memMessage
	states   map[message.Namespace]map[message.Position]message.State
	revision uint64
	keys     map[message.Namespace]map[string]memKey
	log      map[message.Shard][]message.Change
	wake     chan struct{}
}

type memMessage struct {
	body []byte
	meta map[string]string
	at   time.Duration
}

type memKey struct {
	pos message.Position
	at  time.Duration
}

// memStore is the reference fake: the simplest store that meets the
// contract, so the suite is proven runnable before any real store exists.
type memStore struct{ d *memData }

var _ message.Store = (*memStore)(nil)

func newMemData() *memData {
	return &memData{
		limits:   message.Limits{RetentionBackstop: message.MinRetentionBackstop, IdempotencyWindow: 2 * time.Minute},
		seq:      map[message.Shard]uint64{},
		messages: map[message.Namespace]map[message.Position]memMessage{},
		states:   map[message.Namespace]map[message.Position]message.State{},
		keys:     map[message.Namespace]map[string]memKey{},
		log:      map[message.Shard][]message.Change{},
		wake:     make(chan struct{}),
	}
}

func (d *memData) advance(by time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clock += by
}

// check validates only the queue ID: memory has a slot for every shard, so
// no allocation is ever out of range.
func (s *memStore) check(ns message.Namespace) error {
	return ns.Validate()
}

// expire drops messages past the backstop; callers hold the lock.
func (s *memStore) expire() {
	for _, msgs := range s.d.messages {
		maps.DeleteFunc(msgs, func(_ message.Position, m memMessage) bool {
			return s.d.clock-m.at >= s.d.limits.RetentionBackstop
		})
	}
}

func (s *memStore) Append(_ context.Context, ns message.Namespace, in message.AppendInput) (message.AppendResult, error) {
	if err := s.check(ns); err != nil {
		return message.AppendResult{}, err
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if in.IdempotencyKey != "" {
		if k, ok := d.keys[ns][in.IdempotencyKey]; ok && d.clock-k.at < d.limits.IdempotencyWindow {
			return message.AppendResult{Position: k.pos, Duplicate: true}, nil
		}
	}
	d.seq[ns.MessageShard]++
	pos := message.Position(d.seq[ns.MessageShard])
	if d.messages[ns] == nil {
		d.messages[ns] = map[message.Position]memMessage{}
	}
	d.messages[ns][pos] = memMessage{body: slices.Clone(in.Body), meta: maps.Clone(in.Meta), at: d.clock}
	if in.IdempotencyKey != "" {
		if d.keys[ns] == nil {
			d.keys[ns] = map[string]memKey{}
		}
		d.keys[ns][in.IdempotencyKey] = memKey{pos: pos, at: d.clock}
	}
	d.log[ns.MessageShard] = append(d.log[ns.MessageShard], message.Change{
		Namespace: ns, Position: pos, Meta: maps.Clone(in.Meta), Cursor: message.Cursor(pos),
	})
	close(d.wake)
	d.wake = make(chan struct{})
	return message.AppendResult{Position: pos}, nil
}

func (s *memStore) Get(_ context.Context, ns message.Namespace, pos message.Position) (message.Message, error) {
	if err := s.check(ns); err != nil {
		return message.Message{}, err
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.expire()
	m, ok := s.d.messages[ns][pos]
	if !ok {
		return message.Message{}, message.ErrNotFound
	}
	return message.Message{Position: pos, Body: slices.Clone(m.body), Meta: maps.Clone(m.meta)}, nil
}

func (s *memStore) Delete(_ context.Context, ns message.Namespace, pos message.Position) error {
	if err := s.check(ns); err != nil {
		return err
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	delete(s.d.messages[ns], pos)
	return nil
}

func (s *memStore) DeleteThrough(_ context.Context, ns message.Namespace, through message.Position) error {
	if err := s.check(ns); err != nil {
		return err
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	maps.DeleteFunc(s.d.messages[ns], func(p message.Position, _ memMessage) bool { return p <= through })
	return nil
}

func (s *memStore) GetState(_ context.Context, ns message.Namespace, pos message.Position) (message.State, error) {
	if err := s.check(ns); err != nil {
		return message.State{}, err
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	st, ok := s.d.states[ns][pos]
	if !ok {
		return message.State{}, message.ErrNotFound
	}
	return message.State{Value: slices.Clone(st.Value), Revision: st.Revision}, nil
}

func (s *memStore) PutState(_ context.Context, ns message.Namespace, pos message.Position, value []byte, expected message.Revision) (message.Revision, error) {
	if err := s.check(ns); err != nil {
		return 0, err
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.states[ns][pos].Revision != expected {
		return 0, message.ErrRevisionMismatch
	}
	if d.states[ns] == nil {
		d.states[ns] = map[message.Position]message.State{}
	}
	d.revision++
	rev := message.Revision(d.revision)
	d.states[ns][pos] = message.State{Value: slices.Clone(value), Revision: rev}
	return rev, nil
}

func (s *memStore) DeleteState(_ context.Context, ns message.Namespace, pos message.Position, expected message.Revision) error {
	if err := s.check(ns); err != nil {
		return err
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	st, ok := s.d.states[ns][pos]
	switch {
	case !ok:
		return message.ErrNotFound
	case st.Revision != expected:
		return message.ErrRevisionMismatch
	}
	delete(s.d.states[ns], pos)
	return nil
}

func (s *memStore) Feed(_ context.Context, shard message.Shard, after message.Cursor) (message.Feed, error) {
	return &memFeed{d: s.d, shard: shard, after: after}, nil
}

func (s *memStore) DeleteNamespace(_ context.Context, ns message.Namespace) error {
	if err := s.check(ns); err != nil {
		return err
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	delete(s.d.messages, ns)
	delete(s.d.states, ns)
	delete(s.d.keys, ns)
	return nil
}

func (s *memStore) Count(_ context.Context, ns message.Namespace) (message.Usage, error) {
	if err := s.check(ns); err != nil {
		return message.Usage{}, err
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.expire()
	return s.usage(ns), nil
}

func (s *memStore) usage(ns message.Namespace) message.Usage {
	return message.Usage{Messages: uint64(len(s.d.messages[ns])), States: uint64(len(s.d.states[ns]))}
}

func (s *memStore) Namespaces(_ context.Context, shard message.Shard) ([]message.NamespaceUsage, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.expire()
	seen := map[message.Namespace]bool{}
	var out []message.NamespaceUsage
	for _, set := range []map[message.Namespace]bool{nsKeys(s.d.messages), nsKeys(s.d.states)} {
		for ns := range set {
			if ns.MessageShard != shard || seen[ns] {
				continue
			}
			seen[ns] = true
			if u := s.usage(ns); u != (message.Usage{}) {
				out = append(out, message.NamespaceUsage{Queue: ns.Queue, Usage: u})
			}
		}
	}
	return out, nil
}

func nsKeys[V any](m map[message.Namespace]V) map[message.Namespace]bool {
	out := make(map[message.Namespace]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func (s *memStore) Limits() message.Limits { return s.d.limits }

type memFeed struct {
	d     *memData
	shard message.Shard
	after message.Cursor
}

func (f *memFeed) Next(ctx context.Context) (message.Change, error) {
	for {
		if err := ctx.Err(); err != nil {
			return message.Change{}, err
		}
		f.d.mu.Lock()
		for _, c := range f.d.log[f.shard] {
			if c.Cursor <= f.after {
				continue
			}
			f.after = c.Cursor
			if _, live := f.d.messages[c.Namespace][c.Position]; live {
				f.d.mu.Unlock()
				c.Meta = maps.Clone(c.Meta)
				return c, nil
			}
		}
		wake := f.d.wake
		f.d.mu.Unlock()
		select {
		case <-ctx.Done():
			return message.Change{}, ctx.Err()
		case <-wake:
		}
	}
}

func (f *memFeed) Close() error { return nil }
