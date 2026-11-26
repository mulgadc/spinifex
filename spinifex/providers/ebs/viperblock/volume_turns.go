package viperblock

import (
	"encoding/json"
	"sync"

	"github.com/nats-io/nats.go"
)

// maxConcurrentPublishes is the most publishes one node runs at once on the
// socket transport. TCP runs one, because a free port is only reserved once
// nbdkit binds it.
const maxConcurrentPublishes = 8

// volumeTurns gives the operations on one volume a turn each, in the order
// they took a ticket. Operations on different volumes do not wait for each other.
type volumeTurns struct {
	mu   sync.Mutex
	last map[string]*volumeTicket
}

// volumeTicket is one operation's place in its volume's queue.
type volumeTicket struct {
	turns    *volumeTurns
	volume   string
	prev     chan struct{}
	finished chan struct{}
}

// take queues an operation on volume and does not block.
func (t *volumeTurns) take(volume string) *volumeTicket {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]*volumeTicket{}
	}
	ticket := &volumeTicket{turns: t, volume: volume, finished: make(chan struct{})}
	if prev, ok := t.last[volume]; ok {
		ticket.prev = prev.finished
	}
	t.last[volume] = ticket
	return ticket
}

// wait blocks until every earlier ticket for the volume is done.
func (k *volumeTicket) wait() {
	if k.prev != nil {
		<-k.prev
	}
}

// done passes the turn to the next ticket, and drops the volume's entry when
// no ticket is left.
func (k *volumeTicket) done() {
	k.turns.mu.Lock()
	if k.turns.last[k.volume] == k {
		delete(k.turns.last, k.volume)
	}
	k.turns.mu.Unlock()
	close(k.finished)
}

// volumeOpHandler runs handler on its own goroutine once the request's volume
// has its turn. A publish also takes a publish slot. A request that does not
// name a valid volume runs inline.
func (cfg *Config) volumeOpHandler(publish bool, handler providerMsgHandler) nats.MsgHandler {
	traced := tracedProviderHandler(handler)
	return func(msg *nats.Msg) {
		var req struct {
			VolumeID string `json:"volume_id"`
		}
		if json.Unmarshal(msg.Data, &req) != nil || !validVolumeName(req.VolumeID) {
			traced(msg)
			return
		}
		ticket := cfg.volumeTurns.take(req.VolumeID)
		cfg.volumeOps.Go(func() {
			defer ticket.done()
			ticket.wait()
			if publish {
				cfg.publishSlots <- struct{}{}
				defer func() { <-cfg.publishSlots }()
			}
			traced(msg)
		})
	}
}
