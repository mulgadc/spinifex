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
	"github.com/nats-io/nkeys"
)

// StorePort is the port the store guest's NATS listens on.
const StorePort = 4222

// NATSChecker connects with the worker key and asks JetStream for its account
// info. It authenticates the client only: there is no TLS, so a pass says
// something answering at the address accepted the key.
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

// ErrStoreRefusedKey is a store that answered but refused the worker key.
var ErrStoreRefusedKey = errors.New("the store refused the worker key")

// Check passes once JetStream answers for the worker key.
func (c NATSChecker) Check(ctx context.Context, address string, kp nkeys.KeyPair) error {
	if ip := net.ParseIP(address); ip == nil || ip.To4() == nil {
		return errors.New("store address is not an IPv4 address")
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return fmt.Errorf("derive worker public key: %w", err)
	}
	port := c.Port
	if port == 0 {
		port = StorePort
	}
	dial := c.Dial
	if dial == 0 {
		dial = 5 * time.Second
	}
	nc, err := nats.Connect("nats://"+net.JoinHostPort(address, strconv.Itoa(port)),
		nats.Nkey(pub, kp.Sign),
		nats.Timeout(dial),
		nats.NoReconnect(),
		nats.Name("spinifex-sqs-worker-readiness"),
	)
	if err != nil {
		if errors.Is(err, nats.ErrAuthorization) || errors.Is(err, nats.ErrAuthExpired) {
			return fmt.Errorf("%w: %w", ErrStoreRefusedKey, err)
		}
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
