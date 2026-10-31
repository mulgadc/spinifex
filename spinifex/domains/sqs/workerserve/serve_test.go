package workerserve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
)

type postGateway struct {
	outcome string
	err     error
}

func (postGateway) PollWork(context.Context) ([]relay.Work, error) { return nil, nil }

func (g postGateway) CompleteWork(context.Context, string, []byte) (string, error) {
	return g.outcome, g.err
}

type claimingExec struct{ released []relay.Claim }

func (e *claimingExec) Execute(context.Context, relay.Work) ([]byte, []relay.Claim) {
	return []byte(`{"output":[]}`), []relay.Claim{{Account: "123456789012", Region: "ap-southeast-2", Queue: "orders", ReceiptHandle: "h"}}
}

func (e *claimingExec) Release(_ context.Context, c []relay.Claim) error {
	e.released = append(e.released, c...)
	return nil
}

// Claimed messages are released only when the gateway proves the caller
// left before the result arrived. A lost reply, an error or any other answer
// may have reached the caller, so those messages wait out their visibility.
func TestHandle_ReleasesOnlyWhenTheCallerProvablyLeft(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gw      postGateway
		release bool
	}{
		{"caller gone", postGateway{outcome: OutcomeCallerGone}, true},
		{"delivered", postGateway{outcome: OutcomeDelivered}, false},
		{"unknown", postGateway{outcome: "unknown"}, false},
		{"lost reply", postGateway{err: errors.New("connection reset")}, false},
		{"refused", postGateway{outcome: OutcomeCallerGone, err: errors.New("AccessDenied")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &claimingExec{}
			cfg := Config{Gateway: tc.gw, Executor: exec}
			handle(context.Background(), cfg, relay.Work{ID: "w", Op: relay.OpReceiveMessage, Deadline: time.Now().Add(time.Second)}, func(string) {})
			if got := len(exec.released) > 0; got != tc.release {
				t.Fatalf("released = %v, want %v", got, tc.release)
			}
		})
	}
}
