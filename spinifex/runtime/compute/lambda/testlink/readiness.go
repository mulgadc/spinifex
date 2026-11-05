//go:build lambdatestlink

package testlink

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
)

// Readiness admits a function only while d serves the pinned image and its node
// is the cluster's only one, so no other node's gateway or daemon is handed an
// Active function it cannot run.
func (d *Daemon) Readiness(imagePin string, nodes int) function.Readiness {
	return func(context.Context, function.Record) error {
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
