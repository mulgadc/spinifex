// Package projection is network's authorised view onto the vpc.* NATS
// realisation boundary described by contracts/network/v1. It wraps every
// subject in an operation-specific method so callers never pass a subject
// string or a bare event value: the method name states the delivery mode
// (strict, best-effort, unacknowledged) that today's call sites rely on.
//
// This package moves no behaviour: every timeout, retry and barrier here is
// the one spinifex/utils/vpcd_event.go and each call site already had. It
// replaces spinifex/utils/vpcd_event.go, which no longer exists.
package projection

import (
	"encoding/json"
	"fmt"
	"time"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// Client issues vpc.* network-realisation requests over one NATS connection.
type Client struct {
	nc *nats.Conn
}

// New returns a Client that publishes and requests over nc. nc may be nil;
// every method then fails (or, for unacknowledged methods, logs) the same
// way a nil connection fails today.
func New(nc *nats.Conn) *Client {
	return &Client{nc: nc}
}

// request marshals event, sends a synchronous NATS request on subject and
// blocks until vpcd acks with {"success":true} or a failure envelope.
func (c *Client) request(subject string, event any, timeout time.Duration) error {
	if c.nc == nil {
		return fmt.Errorf("%s: nats connection not initialized", subject)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", subject, err)
	}
	resp, err := c.nc.Request(subject, data, timeout)
	if err != nil {
		return fmt.Errorf("%s request: %w", subject, err)
	}
	var result networkv1.AckEnvelope
	if jsonErr := json.Unmarshal(resp.Data, &result); jsonErr != nil {
		return fmt.Errorf("%s: unmarshal response: %w", subject, jsonErr)
	}
	if !result.Success {
		return fmt.Errorf("%s: %s", subject, result.Error)
	}
	return nil
}

// announce publishes event on subject with no reply expected; a nil
// connection, marshal failure or publish failure is logged, not returned.
func (c *Client) announce(subject string, event any) {
	natsmsg.PublishEvent(c.nc, subject, event)
}
