package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/nats-io/nats.go/jetstream"
)

// transitGatewayDrain is level 3's rollback drain: Transit Gateways not yet
// deleted, routes naming one, and VPC or subnet deletion marks, each of which
// an older daemon would drop or misread.
type transitGatewayDrain struct {
	mu     sync.Mutex
	live   func(ctx context.Context) (int, error)
	routes func(ctx context.Context) (int, error)
	marks  func(ctx context.Context) (int, error)
	out    io.Writer
}

var tgwDrain = &transitGatewayDrain{}

func (d *transitGatewayDrain) connect(js jetstream.JetStream, out io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.out = out
	d.live = func(ctx context.Context) (int, error) { return ec2tgw.CountLive(ctx, js) }
	d.routes = func(ctx context.Context) (int, error) { return routetable.CountTransitGatewayRoutes(ctx, js) }
	d.marks = func(ctx context.Context) (int, error) { return ec2vpc.CountDeletionMarks(ctx, js) }
}

func (d *transitGatewayDrain) check(ctx context.Context) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.live == nil || d.routes == nil || d.marks == nil {
		return 0, errors.New("transit gateway drain is not connected")
	}
	live, err := d.live(ctx)
	if err != nil {
		return 0, fmt.Errorf("count transit gateways: %w", err)
	}
	routes, err := d.routes(ctx)
	if err != nil {
		return 0, fmt.Errorf("count transit gateway routes: %w", err)
	}
	marks, err := d.marks(ctx)
	if err != nil {
		return 0, fmt.Errorf("count deletion marks: %w", err)
	}
	if d.out != nil {
		fmt.Fprintf(d.out, "Transit Gateways: %d not deleted, %d route(s) naming one, %d VPC or subnet record(s) with a deletion mark.\n",
			live, routes, marks)
		if live+routes > 0 {
			fmt.Fprintln(d.out, "Delete every Transit Gateway route and Transit Gateway before rolling back: rollback never deletes one.")
		}
		if marks > 0 {
			fmt.Fprintln(d.out, "Retry DeleteVpc or DeleteSubnet for each marked record before rolling back.")
		}
	}
	return live + routes + marks, nil
}
