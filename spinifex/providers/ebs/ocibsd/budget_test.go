package ocibsd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
)

// A publish's attach and device waits must end inside its own bound, and that
// bound inside the client's, or a slow attach reaches the caller as a timeout
// that leaves unknown whether the volume was exported.
func TestPublishWaitsFitTheClientsPublishBudget(t *testing.T) {
	waits := defaultAttachTimeout + defaultPollInterval + defaultDeviceTimeout
	assert.Less(t, waits, defaultPublishTimeout, "the attach and device waits must fit the publish bound")
	assert.Less(t, defaultPublishTimeout, ebsprovider.PublishTimeout, "the publish bound must answer before the client gives up")
}
