package elbv2

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/nats-io/nats.go/jetstream"
)

// RunOVNFixtureIntentHeldReader runs the held-reader fixture round trip with
// the named instance reader that newReader builds on the fixture's JetStream.
func RunOVNFixtureIntentHeldReader(t *testing.T, newReader func(jetstream.JetStream) projection.NamedInstanceLookup) {
	runOVNFixtureIntentHeldReader(t, newReader)
}
