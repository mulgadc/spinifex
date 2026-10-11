package spx

import (
	"context"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"time"

	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	"github.com/nats-io/nats.go"
)

// PromoteImage publishes a promotion request to the spinifex.image.promote NATS
// topic and waits for the daemon to call ec2image.PromoteSystemImage and reply.
func PromoteImage(ctx context.Context, nc *nats.Conn, imageID, accountID string) (*ec2image.PromoteImageResult, error) {
	return natsmsg.NATSRequest[ec2image.PromoteImageResult](ctx, nc, "spinifex.image.promote", ec2image.PromoteImageOpts{ImageID: imageID}, 30*time.Second, accountID)
}
