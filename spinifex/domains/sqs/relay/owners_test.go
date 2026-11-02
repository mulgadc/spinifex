package relay_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerserve"
)

const (
	acct   = "123456789012"
	region = "ap-southeast-2"
)

var taker = relay.Taker{InstanceID: "i-0abc12345def67890", Generation: 1, ClaimID: "claim-0001"}

// served starts real owners on an embedded store behind r, posting each
// result back as the worker loop does.
func served(t *testing.T) (*relay.Relay, relay.Owners) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	owners, err := workerserve.OpenOwners(ctx, js, key)
	if err != nil {
		t.Fatalf("open owners: %v", err)
	}
	r := &relay.Relay{}
	polled, done := context.WithCancel(ctx)
	done()
	r.Take(polled, taker, 1, 1)
	go func() {
		for ctx.Err() == nil {
			for _, w := range r.Take(ctx, taker, 10, 10<<20) {
				body, _ := owners.Execute(ctx, w)
				var res relay.Result
				if err := json.Unmarshal(body, &res); err != nil {
					panic(err)
				}
				_, _ = r.Complete(taker, w.ID, res)
			}
		}
	}()
	return r, owners
}

func callCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Every queue action crosses the wire into the real owner and back, and its
// effect is visible to the next call.
func TestOwners_QueueActionsRoundTrip(t *testing.T) {
	r, _ := served(t)
	ctx := callCtx(t)

	rec, err := r.Create(ctx, queue.CreateRequest{Account: acct, Region: region, Name: "orders", Tags: map[string]string{"team": "a"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, err := r.Resolve(ctx, acct, region, "orders"); err != nil || got.QueueID != rec.QueueID {
		t.Fatalf("resolve = %v, %v; want %s", got.QueueID, err, rec.QueueID)
	}
	if _, err := r.SetAttributes(ctx, acct, region, "orders", map[string]string{"VisibilityTimeout": "45"}); err != nil {
		t.Fatalf("set attributes: %v", err)
	}
	if got, _ := r.Resolve(ctx, acct, region, "orders"); got.Attributes["VisibilityTimeout"] != "45" {
		t.Fatalf("VisibilityTimeout = %q after set", got.Attributes["VisibilityTimeout"])
	}
	if _, err := r.Tag(ctx, acct, region, "orders", map[string]string{"env": "dev"}); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if _, err := r.Untag(ctx, acct, region, "orders", []string{"team"}); err != nil {
		t.Fatalf("untag: %v", err)
	}
	tags, err := r.Tags(ctx, acct, region, "orders")
	if err != nil || len(tags) != 1 || tags["env"] != "dev" {
		t.Fatalf("tags = %v, %v; want only env=dev", tags, err)
	}
	page, err := r.List(ctx, queue.ListRequest{Account: acct, Region: region})
	if err != nil || len(page.Queues) != 1 || page.Queues[0].Name != "orders" {
		t.Fatalf("list = %+v, %v", page, err)
	}
	if _, err := r.Resolve(ctx, acct, region, "missing"); !errors.Is(err, queue.ErrQueueDoesNotExist) {
		t.Fatalf("resolve missing = %v; want QueueDoesNotExist", err)
	}
}

// A receive asking for the AWS maximum claims at most ReceiveLimit, leaving
// the rest visible; Release and ChangeVisibility return claims at once.
func TestOwners_ReceiveClaimsAtMostTheLimitAndReleaseReturnsThem(t *testing.T) {
	r, owners := served(t)
	ctx := callCtx(t)
	if _, err := r.Create(ctx, queue.CreateRequest{Account: acct, Region: region, Name: "jobs"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := range message.MaxReceiveMessages {
		if _, err := r.Send(ctx, message.SendInput{Account: acct, Region: region, Queue: "jobs", Sender: "AIDATEST", Body: fmt.Appendf(nil, "m%d", i)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	maxN, hide := message.MaxReceiveMessages, 300
	in := message.ReceiveInput{Account: acct, Region: region, Queue: "jobs", MaxNumberOfMessages: &maxN, VisibilityTimeout: &hide}
	first, err := r.Receive(ctx, in)
	if err != nil || len(first) == 0 || len(first) > relay.ReceiveLimit {
		t.Fatalf("receive = %d messages, %v; want 1..%d", len(first), err, relay.ReceiveLimit)
	}
	rest, err := r.Receive(ctx, in)
	if err != nil || len(first)+len(rest) != message.MaxReceiveMessages {
		t.Fatalf("second receive = %d, %v; want the %d unclaimed", len(rest), err, message.MaxReceiveMessages-len(first))
	}

	// Everything is now hidden for 300s. Release the first batch's claims
	// directly and the second through ChangeMessageVisibility over the wire.
	claims := make([]relay.Claim, len(first))
	for i, m := range first {
		claims[i] = relay.Claim{Account: acct, Region: region, Queue: "jobs", ReceiptHandle: m.ReceiptHandle}
	}
	if err := owners.Release(ctx, claims); err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, m := range rest {
		if err := r.ChangeVisibility(ctx, message.ChangeVisibilityInput{Account: acct, Region: region, Queue: "jobs", ReceiptHandle: m.ReceiptHandle}); err != nil {
			t.Fatalf("change visibility: %v", err)
		}
	}
	seen := 0
	for seen < message.MaxReceiveMessages {
		got, err := r.Receive(ctx, in)
		if err != nil || len(got) == 0 {
			t.Fatalf("after release saw %d of %d, then %v", seen, message.MaxReceiveMessages, err)
		}
		seen += len(got)
	}
}

// Work the worker cannot decode or does not know is a fault, never a
// success, and claims nothing.
func TestOwners_UnknownOrUndecodableWorkFaults(t *testing.T) {
	_, owners := served(t)
	ctx := callCtx(t)
	for _, w := range []relay.Work{
		{ID: "a", Op: "PurgeEverything", Input: json.RawMessage(`{}`)},
		{ID: "b", Op: relay.OpSendMessage, Input: json.RawMessage(`[`)},
		{ID: "c", Op: relay.OpReceiveMessage, Input: json.RawMessage(`"x"`)},
	} {
		body, claims := owners.Execute(ctx, w)
		var res relay.Result
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("%s: result undecodable: %v", w.Op, err)
		}
		if res.Error == nil || res.Error.Kind != "fault" || res.Output != nil || claims != nil {
			t.Fatalf("%s: result %+v claims %d; want a fault and no claims", w.Op, res, len(claims))
		}
	}
}
