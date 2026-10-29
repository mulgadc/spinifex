package conformance_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message/conformance"
)

const fakeShards = 2

func freshHarness(t *testing.T) conformance.Harness {
	t.Helper()
	d := newMemData()
	return conformance.Harness{
		Store:         &memStore{d: d},
		MessageShards: fakeShards,
		StateShards:   fakeShards,
		Reopen:        func(*testing.T) message.Store { return &memStore{d: d} },
		Advance:       d.advance,
	}
}

func TestReferenceFakeConformance(t *testing.T) {
	conformance.Run(t, freshHarness)
}

// TestReferenceFakeSharedInstanceConformance stands in for a long-lived store
// holding other queues' data: every case must still pass when it is not alone.
func TestReferenceFakeSharedInstanceConformance(t *testing.T) {
	shared := freshHarness(t)
	conformance.Run(t, func(*testing.T) conformance.Harness { return shared })
}

// TestReferenceFakeMinimalHarnessConformance runs with no crash, clock or
// second-shard hooks, as a store that offers none of them would.
func TestReferenceFakeMinimalHarnessConformance(t *testing.T) {
	conformance.Run(t, func(*testing.T) conformance.Harness {
		return conformance.Harness{Store: &memStore{d: newMemData()}, MessageShards: 1, StateShards: 1}
	})
}
