//go:build lambdatestlink

package testlink

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
)

// Readiness admits a function the test link can run, and only while d serves
// the pinned image on the cluster's only node, so no gateway or daemon is
// handed an Active function it cannot run.
func (d *Daemon) Readiness(imagePin string, nodes int) function.Readiness {
	return func(_ context.Context, rec function.Record) error {
		if !validHandler(rec.Handler) {
			return notReady("The function cannot run here: test-link Lambda runs only a handler of 1 to 128 characters with no whitespace.")
		}
		if rec.Code.Size <= 0 || rec.Code.Size > MaxCodeBytes {
			return notReady("The function cannot run here: its code is larger than test-link Lambda accepts.")
		}
		if nodes != 1 {
			return notReady("The function cannot run here: test-link Lambda activates only on a single-node cluster.")
		}
		if !d.Serving() || d.Image == nil || d.Image.Digest != imagePin {
			return notReady(function.ReasonCreating)
		}
		return nil
	}
}

// notReady is an unmet activation condition, worded as the StateReason.
type notReady string

func (n notReady) Error() string { return string(n) }
