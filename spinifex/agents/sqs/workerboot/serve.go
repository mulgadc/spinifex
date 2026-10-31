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

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
)

// ServeConfig makes the ready step also serve SQS owner calls.
type ServeConfig struct {
	Connector Connector
	// Run serves until ctx ends against the store's JetStream, with the
	// receipt key from the worker disk; it must not retain the key.
	Run func(ctx context.Context, region, gatewayURL string, js jetstream.JetStream, receiptKey []byte) error
}

// Connector opens the store's JetStream with the worker key.
type Connector interface {
	Connect(ctx context.Context, address string, kp nkeys.KeyPair) (jetstream.JetStream, func(), error)
}

// NATSConnector connects to the store guest's NATS; like NATSChecker it
// authenticates the client only.
type NATSConnector struct {
	// Port overrides StorePort, for tests against a local server.
	Port int
}

var _ Connector = NATSConnector{}

func (c NATSConnector) Connect(_ context.Context, address string, kp nkeys.KeyPair) (jetstream.JetStream, func(), error) {
	if ip := net.ParseIP(address); ip == nil || ip.To4() == nil {
		return nil, nil, errors.New("store address is not an IPv4 address")
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return nil, nil, fmt.Errorf("derive worker public key: %w", err)
	}
	port := c.Port
	if port == 0 {
		port = StorePort
	}
	nc, err := nats.Connect("nats://"+net.JoinHostPort(address, strconv.Itoa(port)),
		nats.Nkey(pub, kp.Sign),
		nats.Timeout(5*time.Second),
		nats.MaxReconnects(-1),
		nats.Name("spinifex-sqs-worker"),
	)
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
// open them. It never creates a key: both keys must already be on the disk.
func serve(ctx context.Context, cfg ReadyConfig, c contract, d StoreDescriber, keys workerkey.Store) {
	last := ""
	for ctx.Err() == nil {
		err := serveOnce(ctx, cfg, c, d, keys)
		if ctx.Err() != nil {
			return
		}
		if state := fmt.Sprintf("SQS serving stopped: %v", err); state != last {
			cfg.Log(state)
			last = state
		}
		delay := cfg.Wait
		if !pending(err) && !retryable(err) && !errors.Is(err, errStorePending) {
			delay = orDefault(cfg.Refused, 5*time.Minute)
		}
		select {
		case <-ctx.Done():
		case <-time.After(max(delay, time.Millisecond)):
		}
	}
}

func serveOnce(ctx context.Context, cfg ReadyConfig, c contract, d StoreDescriber, keys workerkey.Store) error {
	callCtx, cancel := context.WithTimeout(ctx, orDefault(cfg.CallTimeout, 30*time.Second))
	defer cancel()
	st, err := d.DescribeStore(callCtx)
	if err != nil {
		return err
	}
	if !st.Bound {
		return errStorePending
	}
	kp, err := keys.ForExistingStore(st.PublicKey)
	if err != nil {
		return err
	}
	defer kp.Wipe()
	receipt, err := keys.ReceiptKey()
	if err != nil {
		return err
	}
	defer clear(receipt)
	js, closeConn, err := cfg.Serve.Connector.Connect(callCtx, st.Address, kp)
	if err != nil {
		return err
	}
	defer closeConn()
	cfg.Log("serving SQS owner calls from the store at " + st.Address)
	return cfg.Serve.Run(ctx, c.region, c.gatewayURL, js, receipt)
}
