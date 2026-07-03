package spx

import (
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// PromoteImage publishes a promotion request to the spinifex.image.promote NATS
// topic and waits for the daemon to call admin.PromoteSystemImage and reply.
func PromoteImage(nc *nats.Conn, imageID, accountID string) (*admin.PromoteImageOutput, error) {
	input := admin.PromoteImageInput{ImageID: imageID}
	return utils.NATSRequest[admin.PromoteImageOutput](nc, "spinifex.image.promote", input, 30*time.Second, accountID)
}
