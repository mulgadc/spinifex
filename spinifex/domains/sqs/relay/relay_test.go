package relay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/bluebottle/pkg/sigv4"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
)

var worker = Taker{InstanceID: "i-0abc12345def67890", Generation: 1, ClaimID: "claim-0001"}

// takeOne waits for the caller's item to be queued and takes it.
func takeOne(t *testing.T, r *Relay, as Taker) Work {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := r.Take(ctx, as, 1, 1<<20)
	if len(got) != 1 {
		t.Fatalf("took %d items", len(got))
	}
	return got[0]
}

// present marks a worker as having polled, without leaving a poll waiting.
func present(r *Relay) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Take(ctx, worker, 1, 1)
}

// Only the identity that took an item may post it, and the caller gets the
// owner's error with its sentinel intact.
func TestComplete_DeliversOnlyFromTheTaker(t *testing.T) {
	r := &Relay{}
	present(r)
	done := make(chan error, 1)
	go func() {
		_, err := r.Resolve(context.Background(), "123456789012", "ap-southeast-2", "orders")
		done <- err
	}()
	w := takeOne(t, r, worker)
	res := Result{Error: encodeError(queue.ErrQueueDoesNotExist)}

	successor := worker
	successor.Generation = 2
	if _, err := r.Complete(successor, w.ID, res); !errors.Is(err, ErrNotTaker) {
		t.Fatalf("post by another identity = %v", err)
	}
	if out, err := r.Complete(worker, w.ID, res); err != nil || out != Delivered {
		t.Fatalf("post = %v, %v", out, err)
	}
	if err := <-done; !errors.Is(err, queue.ErrQueueDoesNotExist) {
		t.Fatalf("caller got %v", err)
	}
	if out, _ := r.Complete(worker, w.ID, res); out != Unknown {
		t.Fatalf("second post = %v, want Unknown", out)
	}
}

// A post after the caller left is CallerGone, the one outcome that lets the
// worker release what it claimed; an id never handed out is Unknown.
func TestComplete_CallerGoneOnlyAfterTheCallerLeft(t *testing.T) {
	r := &Relay{}
	present(r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Receive(ctx, message.ReceiveInput{Account: "123456789012", Region: "ap-southeast-2", Queue: "orders"})
		done <- err
	}()
	w := takeOne(t, r, worker)
	cancel()
	if err := <-done; !errors.Is(err, message.ErrUnavailable) {
		t.Fatalf("caller got %v", err)
	}
	out, err := r.Complete(worker, w.ID, Result{Output: []byte("[]")})
	if err != nil || out != CallerGone {
		t.Fatalf("late post = %v, %v", out, err)
	}
	if out, _ := r.Complete(worker, strings.Repeat("0", 32), Result{Output: []byte("[]")}); out != Unknown {
		t.Fatalf("unknown id = %v", out)
	}
}

// With no worker polling, a call fails at once and queues nothing.
func TestCall_NoWorkerFailsAtOnce(t *testing.T) {
	r := &Relay{PollerStale: time.Minute}
	start := time.Now()
	_, err := r.Send(context.Background(), message.SendInput{Account: "123456789012", Region: "ap-southeast-2", Queue: "orders"})
	if !errors.Is(err, message.ErrUnavailable) || !errors.Is(err, ErrNoWorker) || time.Since(start) > time.Second {
		t.Fatalf("send = %v after %s", err, time.Since(start))
	}
	if len(r.items) != 0 {
		t.Fatal("a call was queued with no worker")
	}
}

// ReceiveLimit messages of the largest size, every byte in values that JSON
// escapes, still fit one post.
func TestReceiveLimit_WorstCaseFitsOnePost(t *testing.T) {
	if ReceiveLimit < 1 {
		t.Fatalf("ReceiveLimit = %d", ReceiveLimit)
	}
	var attrs []message.Attribute
	used := 0
	for i := range 10 {
		v := strings.Repeat("\" ", 4096)
		name := "attr" + string(rune('a'+i))
		attrs = append(attrs, message.Attribute{Name: name, Value: message.AttributeValue{DataType: "String", StringValue: &v}})
		used += len(name) + len("String") + len(v)
	}
	set, err := message.NewAttributes(attrs)
	if err != nil {
		t.Fatal(err)
	}
	msgs := make([]message.Received, ReceiveLimit)
	for i := range msgs {
		msgs[i] = message.Received{
			MessageID: strings.Repeat("m", 36), ReceiptHandle: strings.Repeat("h", 1024),
			Body: []byte(strings.Repeat("\"", message.MaxMessageSize-used)), MD5OfBody: strings.Repeat("0", 32),
			Attributes: set, MD5OfMessageAttributes: strings.Repeat("0", 32), SenderID: strings.Repeat("s", 128),
			SentAt: time.Now(), FirstReceive: time.Now(), Deadline: time.Now(), ReceiveCount: 1 << 62,
		}
	}
	out, err := marshal(toWireReceived(msgs))
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalResult(Result{Output: out})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > sigv4.MaxPayloadLen {
		t.Fatalf("%d messages encode to %d bytes, over %d", ReceiveLimit, len(body), sigv4.MaxPayloadLen)
	}
	back, err := fromWireReceived(toWireReceived(msgs))
	if err != nil || string(back[0].Body) != string(msgs[0].Body) || back[0].Attributes.MD5() != set.MD5() {
		t.Fatalf("round trip changed the message: %v", err)
	}
}
