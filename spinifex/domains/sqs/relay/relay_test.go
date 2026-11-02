package relay

import (
	"context"
	"errors"
	"fmt"
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

// While the service is not admitted, a call fails at once even with a worker
// polling, and nothing reaches the worker.
func TestCall_NotAdmittedNeverReachesTheWorker(t *testing.T) {
	r := &Relay{PollerStale: time.Minute, Admit: func(context.Context) bool { return false }}
	present(r)
	_, err := r.Send(context.Background(), message.SendInput{Account: "123456789012", Region: "ap-southeast-2", Queue: "orders"})
	if !errors.Is(err, message.ErrUnavailable) || !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("send = %v", err)
	}
	if len(r.items) != 0 {
		t.Fatal("a call was queued while not admitted")
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

// An uncertain write wrapped with a client fault stays unavailable, for both
// owners, so the caller is never told a write was settled when it was not.
func TestEncodeError_UnavailableWinsOverClientFaults(t *testing.T) {
	for _, tc := range []struct{ unavailable, fault error }{
		{message.ErrUnavailable, message.ErrInvalidParameter},
		{message.ErrUnavailable, message.ErrReceiptInvalid},
		{queue.ErrUnavailable, queue.ErrInvalidAttributeValue},
		{queue.ErrUnavailable, queue.ErrQueueDoesNotExist},
	} {
		got := decodeError(encodeError(errors.Join(tc.fault, tc.unavailable)))
		if !errors.Is(got, tc.unavailable) || errors.Is(got, tc.fault) {
			t.Fatalf("%v + %v decoded as %v", tc.fault, tc.unavailable, got)
		}
	}
}

// Owner and backend text never crosses the wire: the caller sees only the
// fixed text for the kind.
func TestEncodeError_CarriesNoOwnerText(t *testing.T) {
	const secret = "AQEBsecretReceiptHandle"
	wire := encodeError(fmt.Errorf("%w: handle %s rejected", message.ErrReceiptInvalid, secret))
	body, err := MarshalResult(Result{Error: wire})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeError(wire)
	if strings.Contains(string(body), secret) || strings.Contains(got.Error(), secret) || !errors.Is(got, message.ErrReceiptInvalid) {
		t.Fatalf("wire %s, decoded %q", body, got)
	}
}

// answer runs one call through the relay with the worker posting output.
func answer(t *testing.T, output string, call func(r *Relay) error) error {
	t.Helper()
	r := &Relay{}
	present(r)
	done := make(chan error, 1)
	go func() { done <- call(r) }()
	w := takeOne(t, r, worker)
	if _, err := r.Complete(worker, w.ID, Result{Output: []byte(output)}); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// A malformed, empty or mismatched success from the worker is reported as
// unavailable, never as a confirmed send, receive, delete or queue.
func TestRelay_IncompleteSuccessIsUnavailable(t *testing.T) {
	const acct, region = "123456789012", "ap-southeast-2"
	send := func(r *Relay) error {
		_, err := r.Send(context.Background(), message.SendInput{Account: acct, Region: region, Queue: "orders", Body: []byte("hi")})
		return err
	}
	goodMD5 := md5Of([]byte("hi"))
	for name, out := range map[string]string{
		"null":         "null",
		"empty":        "{}",
		"no id":        `{"MD5OfBody":"` + goodMD5 + `"}`,
		"wrong md5":    `{"MessageID":"m","MD5OfBody":"` + md5Of([]byte("other")) + `"}`,
		"attrs md5":    `{"MessageID":"m","MD5OfBody":"` + goodMD5 + `","MD5OfMessageAttributes":"` + goodMD5 + `"}`,
		"not an array": `{"MessageID":"m"`,
		"no position":  `{"MessageID":"m","MD5OfBody":"` + goodMD5 + `","EnqueuedAt":"2026-10-31T00:00:00Z"}`,
		"no enqueue":   `{"MessageID":"m","MD5OfBody":"` + goodMD5 + `","Position":7}`,
	} {
		if err := answer(t, out, send); !errors.Is(err, message.ErrUnavailable) {
			t.Fatalf("send %s = %v", name, err)
		}
	}
	const done = `,"Position":7,"EnqueuedAt":"2026-10-31T00:00:00Z"}`
	if err := answer(t, `{"MessageID":"m","MD5OfBody":"`+goodMD5+`"`+done, send); err != nil {
		t.Fatalf("complete send = %v", err)
	}

	const times = `,"SentAt":"2026-10-31T00:00:00Z","FirstReceive":"2026-10-31T00:00:00Z","Deadline":"2026-10-31T00:01:00Z","ReceiveCount":1`
	full := `{"MessageID":"m","ReceiptHandle":"h","Body":"aGk=","MD5OfBody":"` + goodMD5 + `","SenderID":"AIDA"` + times + `}`
	recv := func(r *Relay) error {
		_, err := r.Receive(context.Background(), message.ReceiveInput{Account: acct, Region: region, Queue: "orders"})
		return err
	}
	for name, out := range map[string]string{
		"null":      "null",
		"no handle": `[{"MessageID":"m","Body":"aGk=","MD5OfBody":"` + goodMD5 + `"}]`,
		"bad md5":   `[{"MessageID":"m","ReceiptHandle":"h","Body":"aGk=","MD5OfBody":"` + md5Of(nil) + `"}]`,
		"no sender": `[{"MessageID":"m","ReceiptHandle":"h","Body":"aGk=","MD5OfBody":"` + goodMD5 + `"` + times + `}]`,
		"too many":  `[` + full + `,` + full + `]`,
	} {
		if err := answer(t, out, recv); !errors.Is(err, message.ErrUnavailable) {
			t.Fatalf("receive %s = %v", name, err)
		}
	}

	if err := answer(t, `[`+full+`]`, recv); err != nil {
		t.Fatalf("complete receive = %v", err)
	}

	del := func(r *Relay) error {
		return r.Delete(context.Background(), message.DeleteInput{Account: acct, Region: region, Queue: "orders", ReceiptHandle: "h"})
	}
	if err := answer(t, "null", del); !errors.Is(err, message.ErrUnavailable) {
		t.Fatalf("delete null = %v", err)
	}

	resolve := func(r *Relay) error {
		_, err := r.Resolve(context.Background(), acct, region, "orders")
		return err
	}
	for name, out := range map[string]string{
		"null":          "null",
		"no queue id":   `{"account":"` + acct + `","region":"` + region + `","name":"orders"}`,
		"other account": `{"queueId":"q1","account":"210987654321","region":"` + region + `","name":"orders"}`,
		"other name":    `{"queueId":"q1","account":"` + acct + `","region":"` + region + `","name":"payments"}`,
		"deleting":      `{"queueId":"q1","account":"` + acct + `","region":"` + region + `","name":"orders","state":"deleting"}`,
	} {
		if err := answer(t, out, resolve); !errors.Is(err, queue.ErrUnavailable) {
			t.Fatalf("resolve %s = %v", name, err)
		}
	}
}

// Pending work is bounded by bytes as well as count: a call that would push
// the held inputs past the bound fails at once and holds nothing.
func TestCall_PendingBytesAreBounded(t *testing.T) {
	r := &Relay{MaxPendingBytes: 64 << 10, CallLimit: time.Second}
	present(r)
	big := message.SendInput{Account: "123456789012", Region: "ap-southeast-2", Queue: "orders", Body: make([]byte, 40<<10)}
	go func() { _, _ = r.Send(context.Background(), big) }()
	first := takeOne(t, r, worker)
	start := time.Now()
	if _, err := r.Send(context.Background(), big); !errors.Is(err, ErrNoWorker) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("second send = %v after %s", err, time.Since(start))
	}
	if _, err := r.Complete(worker, first.ID, Result{Error: encodeError(message.ErrUnavailable)}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	held := r.bytes
	r.mu.Unlock()
	if held != 0 {
		t.Fatalf("bytes held after completion = %d", held)
	}
}
