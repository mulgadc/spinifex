package readiness

import "github.com/nats-io/nats.go/jetstream"

// SetBeforeWriteHook runs f with the operation name just before each record
// write is sent, so a test can pause a writer between its read and its write.
func SetBeforeWriteHook(c *Control, f func(op string)) { c.beforeWrite = f }

// SetAfterReadHook runs f after each record read answers and before it is
// checked or interpreted, so a test can disturb a binding during a read.
func SetAfterReadHook(c *Control, f func()) { c.afterRead = f }

// SetInitStepHook runs f at each named point between initialization steps, so
// a test can pause or disturb an initializer there.
func SetInitStepHook(in *Initializer, f func(step string)) { in.hook = f }

// New returns an unguarded Control over an already-opened kv, for tests of the
// record rules alone; js serves leader reads.
func New(js jetstream.JetStream, kv KV) *Control {
	return &Control{js: js, kv: kv}
}
