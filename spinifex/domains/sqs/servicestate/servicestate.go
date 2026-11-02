// Package servicestate is the operator's durable per-Region choice of whether
// SQS should serve. It is platform service configuration only: no queue or
// message data is kept here, and an absent record means disabled.
package servicestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/nats-io/nats.go/jetstream"
)

// Bucket is the SQS domain's KV bucket for per-Region desired state.
const Bucket = "spinifex-sqs-service"

// Desired is what the operator asked for, not what is running.
type Desired string

const (
	Enabled  Desired = "enabled"
	Disabled Desired = "disabled"
)

// Record is one Region's desired state. Generation rises on every change.
type Record struct {
	Region     string    `json:"region"`
	Desired    Desired   `json:"desired"`
	Node       string    `json:"node,omitempty"`
	Generation uint64    `json:"generation"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

var (
	// ErrNodeConflict is an enable naming a node other than the enabled one;
	// the store and worker are pinned to the node that holds their volumes.
	ErrNodeConflict = errors.New("SQS is enabled on another node")
	// ErrConflict is a concurrent change that kept winning the write.
	ErrConflict = errors.New("SQS service state changed concurrently")

	regionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	nodePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Store reads and writes the records.
type Store struct{ KV jetstream.KeyValue }

// Open opens the bucket, creating it when absent; the operator path uses it.
func Open(ctx context.Context, js jetstream.JetStream) (Store, error) {
	kv, err := sqsjs.OpenDurableKV(ctx, js, Bucket, "SQS service desired state per Region")
	if err != nil {
		return Store{}, err
	}
	return Store{KV: kv}, nil
}

// Bind binds the bucket without creating it.
func Bind(ctx context.Context, js jetstream.JetStream) (Store, error) {
	kv, err := sqsjs.BindDurableKV(ctx, js, Bucket)
	if err != nil {
		return Store{}, err
	}
	return Store{KV: kv}, nil
}

func key(region string) string { return "region." + region }

// Get returns region's record, or a disabled one when none is stored.
func (s Store) Get(ctx context.Context, region string) (Record, error) {
	rec, _, err := s.get(ctx, region)
	return rec, err
}

func (s Store) get(ctx context.Context, region string) (Record, uint64, error) {
	if !regionPattern.MatchString(region) {
		return Record{}, 0, errors.New("invalid region")
	}
	e, err := s.KV.Get(ctx, key(region))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Record{Region: region, Desired: Disabled}, 0, nil
	}
	if err != nil {
		return Record{}, 0, err
	}
	var rec Record
	if err := json.Unmarshal(e.Value(), &rec); err != nil || rec.Region != region ||
		(rec.Desired != Enabled && rec.Desired != Disabled) {
		return Record{}, 0, fmt.Errorf("SQS service record for %s is unreadable", region)
	}
	return rec, e.Revision(), nil
}

// Set records desired for region. Repeating the current state writes nothing.
// Enabling names the node; disabling keeps it, since the volumes stay there.
func (s Store) Set(ctx context.Context, region string, desired Desired, node string, now time.Time) (Record, error) {
	if desired != Enabled && desired != Disabled {
		return Record{}, errors.New("invalid desired state")
	}
	if desired == Enabled && !nodePattern.MatchString(node) {
		return Record{}, errors.New("invalid node")
	}
	for range 5 {
		cur, rev, err := s.get(ctx, region)
		if err != nil {
			return Record{}, err
		}
		if desired == Enabled && cur.Node != "" && cur.Node != node {
			return cur, ErrNodeConflict
		}
		if cur.Desired == desired && (desired == Disabled || cur.Node == node) {
			return cur, nil
		}
		next := Record{Region: region, Desired: desired, Node: cur.Node, Generation: cur.Generation + 1, UpdatedAt: now.UTC()}
		if desired == Enabled {
			next.Node = node
		}
		b, err := json.Marshal(next)
		if err != nil {
			return Record{}, err
		}
		if rev == 0 {
			_, err = s.KV.Create(ctx, key(region), b)
		} else {
			_, err = s.KV.Update(ctx, key(region), b, rev)
		}
		if err == nil {
			return next, nil
		}
		if !errors.Is(err, jetstream.ErrKeyExists) && !errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return Record{}, err
		}
	}
	return Record{}, ErrConflict
}

// Gate answers whether region's SQS should take tenant requests, rereading
// the record at most every TTL. Any read failure answers no.
type Gate struct {
	JS     jetstream.JetStream
	Region string
	TTL    time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	checked time.Time
	open    bool
}

// Open reports whether the record says enabled.
func (g *Gate) Open(ctx context.Context) bool {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.checked.IsZero() && now().Sub(g.checked) < g.TTL {
		return g.open
	}
	g.open = false
	if st, err := Bind(ctx, g.JS); err == nil {
		rec, err := st.Get(ctx, g.Region)
		g.open = err == nil && rec.Desired == Enabled
	}
	g.checked = now()
	return g.open
}
