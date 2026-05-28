package dhcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/nats-io/nats.go"
)

// NATSClient is the daemon-side wrapper around vpcd's Manager. It
// marshals acquire/release requests over NATS request/reply and
// unmarshals the Lease returned by the bridge-owning vpcd. Used by
// ExternalIPAM (Q4) for DHCP-sourced pools.
type NATSClient struct {
	nc      *nats.Conn
	timeout time.Duration
}

// NewNATSClient wraps nc with the given RPC timeout. timeout <= 0
// defaults to 60s — DORA + raw_offer/ack persistence can take seconds
// on a busy upstream server.
func NewNATSClient(nc *nats.Conn, timeout time.Duration) *NATSClient {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &NATSClient{nc: nc, timeout: timeout}
}

// AcquireParams is the wire-level acquire payload. Bridge/HWAddr are
// passed through to the vpcd-side Client.Acquire; Purpose/PoolName/
// VPCID let the manager persist the lease against the right pool
// record.
type AcquireParams struct {
	Bridge      string
	ClientID    string
	Hostname    string
	VendorClass string
	HWAddr      net.HardwareAddr
	Purpose     string
	PoolName    string
	VPCID       string
}

// RequestAcquire issues an idempotent acquire RPC. Retries with the
// same ClientID return the same lease (vpcd's Manager short-circuits
// to the persisted KV record).
func (c *NATSClient) RequestAcquire(ctx context.Context, p AcquireParams) (*Lease, error) {
	if c == nil || c.nc == nil {
		return nil, errors.New("dhcp NATSClient: nil conn")
	}
	if p.ClientID == "" {
		return nil, errors.New("dhcp NATSClient: ClientID required")
	}
	body, err := json.Marshal(acquireWireRequest{
		Bridge:      p.Bridge,
		ClientID:    p.ClientID,
		Hostname:    p.Hostname,
		VendorClass: p.VendorClass,
		HWAddr:      hwToString(p.HWAddr),
		Purpose:     p.Purpose,
		PoolName:    p.PoolName,
		VPCID:       p.VPCID,
	})
	if err != nil {
		return nil, fmt.Errorf("encode acquire request: %w", err)
	}

	rpcCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	msg, err := c.nc.RequestWithContext(rpcCtx, TopicAcquire, body)
	if err != nil {
		return nil, fmt.Errorf("dhcp acquire RPC: %w", err)
	}
	var reply acquireWireReply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return nil, fmt.Errorf("decode acquire reply: %w", err)
	}
	if reply.Error != "" {
		return nil, errors.New(reply.Error)
	}
	return fromWireLease(reply.Lease)
}

// RequestRelease issues an idempotent release RPC. Unknown client-ids
// return nil (vpcd treats them as already-released).
func (c *NATSClient) RequestRelease(ctx context.Context, clientID string) error {
	if c == nil || c.nc == nil {
		return errors.New("dhcp NATSClient: nil conn")
	}
	if clientID == "" {
		return errors.New("dhcp NATSClient: clientID required")
	}
	body, err := json.Marshal(releaseWireRequest{ClientID: clientID})
	if err != nil {
		return fmt.Errorf("encode release request: %w", err)
	}

	rpcCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	msg, err := c.nc.RequestWithContext(rpcCtx, TopicRelease, body)
	if err != nil {
		return fmt.Errorf("dhcp release RPC: %w", err)
	}
	var reply releaseWireReply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return fmt.Errorf("decode release reply: %w", err)
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	return nil
}

func (c *NATSClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}
