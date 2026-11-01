package workerboot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
)

// ServeConfig makes the ready step also serve SQS owner calls.
type ServeConfig struct {
	Connector Connector
	// Run serves until ctx ends against the store's JetStream, with the
	// receipt key from the worker disk; it must not retain the key.
	Run func(ctx context.Context, region, gatewayURL string, js jetstream.JetStream, receiptKey []byte) error
}

// Connector opens the guest's local JetStream.
type Connector interface {
	Connect(ctx context.Context) (jetstream.JetStream, func(), error)
}

// NATSConnector connects to the guest's loopback SQS NATS server.
type NATSConnector struct {
	// Port overrides StorePort, for tests against a local server.
	Port int
}

var _ Connector = NATSConnector{}

// ErrUnsupportedTopology refuses serving, and so creating any store state,
// unless the gateway reports a single-node host cluster.
var ErrUnsupportedTopology = errors.New("SQS serving supports only a single-node host cluster")

// ErrReceiptPending refuses serving while the platform has not recorded the
// receipt key; the boot step records it before the store first starts.
var ErrReceiptPending = errors.New("the receipt key is not yet recorded")

func (c NATSConnector) Connect(_ context.Context) (jetstream.JetStream, func(), error) {
	nc, err := nats.Connect(localURL(c.Port), nats.Timeout(5*time.Second), nats.MaxReconnects(-1),
		nats.Name("spinifex-sqs-worker"))
	if err != nil {
		return nil, nil, &StoreUnreachableError{Err: err}
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, &StoreUnreachableError{Err: err}
	}
	return js, nc.Close, nil
}

// serve runs the owners until ctx ends, starting again after any failure to
// open them. It never creates a key: the receipt key must be on the disk.
func serve(ctx context.Context, cfg ReadyConfig, c contract, keys workerkey.Store) {
	last := ""
	for ctx.Err() == nil {
		err := serveOnce(ctx, cfg, c, keys)
		if ctx.Err() != nil {
			return
		}
		if state := fmt.Sprintf("SQS serving stopped: %v", err); state != last {
			cfg.Log(state)
			last = state
		}
		delay := cfg.Wait
		if !pending(err) && !retryable(err) && !errors.Is(err, ErrReceiptPending) {
			delay = orDefault(cfg.Refused, 5*time.Minute)
		}
		select {
		case <-ctx.Done():
		case <-time.After(max(delay, time.Millisecond)):
		}
	}
}

func serveOnce(ctx context.Context, cfg ReadyConfig, c contract, keys workerkey.Store) error {
	callCtx, cancel := context.WithTimeout(ctx, orDefault(cfg.CallTimeout, 30*time.Second))
	defer cancel()
	if cfg.NewClaimer == nil {
		return errors.New("no gateway client is configured")
	}
	claimer, err := cfg.NewClaimer(c.region, c.gatewayURL)
	if err != nil {
		return fmt.Errorf("build gateway client: %w", err)
	}
	d, err := claimer.Claim(callCtx)
	if err != nil {
		return err
	}
	if d.HostNodes != 1 {
		return fmt.Errorf("%w: the gateway reports %d host nodes", ErrUnsupportedTopology, d.HostNodes)
	}
	if d.Pending {
		return ErrReceiptPending
	}
	receipt, err := keys.ForRecorded(d.Fingerprint)
	if err != nil {
		return err
	}
	defer clear(receipt)
	js, closeConn, err := cfg.Serve.Connector.Connect(callCtx)
	if err != nil {
		return err
	}
	defer closeConn()
	cfg.Log("serving SQS owner calls from the local store")
	return cfg.Serve.Run(ctx, c.region, c.gatewayURL, js, receipt)
}
