package spx

import (
	"context"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// PromoteImage publishes a promotion request to the spinifex.image.promote NATS
// topic and waits for the daemon to call admin.PromoteSystemImage and reply.
func PromoteImage(ctx context.Context, nc *nats.Conn, imageID, accountID string) (*admin.PromoteImageResult, error) {
	return utils.NatsRequest[admin.PromoteImageResult](ctx, nc, "spinifex.image.promote", admin.PromoteImageOpts{ImageID: imageID}, 30*time.Second, accountID)
}
