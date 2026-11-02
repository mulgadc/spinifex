package workerboot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// StorePort is the port the guest's own SQS NATS server listens on, bound
// to loopback only; nothing outside the guest can reach it.
const StorePort = 4222

// NATSChecker asks the guest's loopback JetStream for its account info. A
// pass says the local store is up and serving JetStream.
type NATSChecker struct {
	// Port overrides StorePort, for tests against a local server.
	Port int
	// Dial bounds each connection attempt; zero is five seconds.
	Dial time.Duration
}

var _ Checker = NATSChecker{}

// StoreUnreachableError is a store that did not answer yet; it is retried.
type StoreUnreachableError struct{ Err error }

func (e *StoreUnreachableError) Error() string   { return "store not answering: " + e.Err.Error() }
func (e *StoreUnreachableError) Unwrap() error   { return e.Err }
func (e *StoreUnreachableError) Retryable() bool { return true }

func localURL(port int) string {
	if port == 0 {
		port = StorePort
	}
	return "nats://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// Check passes once the local JetStream answers.
func (c NATSChecker) Check(ctx context.Context) error {
	dial := c.Dial
	if dial == 0 {
		dial = 5 * time.Second
	}
	nc, err := nats.Connect(localURL(c.Port), nats.Timeout(dial), nats.NoReconnect(),
		nats.Name("spinifex-sqs-worker-readiness"))
	if err != nil {
		return &StoreUnreachableError{Err: err}
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return &StoreUnreachableError{Err: err}
	}
	callCtx, cancel := context.WithTimeout(ctx, dial)
	defer cancel()
	if _, err := js.AccountInfo(callCtx); err != nil {
		return &StoreUnreachableError{Err: fmt.Errorf("jetstream account info: %w", err)}
	}
	return nil
}

var errNoChecker = errors.New("no store checker is configured")
