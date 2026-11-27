package elbv2_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/nats-io/nats.go/jetstream"
)

// The three operations answer within their deadlines while a reader of the
// owner key holds its updates, and the daemon's named reader admits the
// registration from the live record.
func TestOVNFixtureIntent_RequestsDoNotWaitForAHeldReader(t *testing.T) {
	elbv2.RunOVNFixtureIntentHeldReader(t, func(js jetstream.JetStream) projection.NamedInstanceLookup {
		return daemon.NewNamedInstanceReader(js)
	})
}
